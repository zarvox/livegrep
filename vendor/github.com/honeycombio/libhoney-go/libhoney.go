// Copyright 2016 Honeycomb, Hound Technology, Inc. All rights reserved.
// Use of this source code is governed by the Apache License 2.0
// license that can be found in the LICENSE file.

package libhoney

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"reflect"
	"strings"
	"sync"
	"time"

	"gopkg.in/alexcesaro/statsd.v2"
)

const (
	defaultSampleRate = 1
	defaultAPIHost    = "https://api.honeycomb.io/"
	version           = "1.1.0"
)

var (
	ptrKinds = []reflect.Kind{reflect.Ptr, reflect.Slice, reflect.Map}
)

// UserAgentAddition is a variable set at compile time via -ldflags to allow you
// to augment the "User-Agent" header that libhoney sends along with each event.
// The default User-Agent is "libhoney-go/1.1.0". If you set this variable, its
// contents will be appended to the User-Agent string, separated by a space. The
// expected format is product-name/version, eg "myapp/1.0"
var UserAgentAddition string

type Config struct {

	// WriteKey is the Honeycomb authentication token. If it is specified during
	// libhoney initialization, it will be used as the default write key for all
	// events. If absent, write key must be explicitly set on a builder or
	// event. Find your team write key at https://ui.honeycomb.io/account
	WriteKey string

	// Dataset is the name of the Honeycomb dataset to which to send these events.
	// If it is specified during libhoney initialization, it will be used as the
	// default dataset for all events. If absent, dataset must be explicitly set
	// on a builder or event.
	Dataset string

	// SampleRate is the rate at which to sample this event. Default is 1,
	// meaning no sampling. If you want to send one event out of every 250 times
	// Send() is called, you would specify 250 here.
	SampleRate uint

	// APIHost is the hostname for the Honeycomb API server to which to send this
	// event. default: https://api.honeycomb.io/
	APIHost string

	// TODO add logger in an agnostic way

	// BlockOnSend determines if libhoney should block or drop packets that exceed
	// the size of the send channel (set by PendingWorkCapacity). Defaults to
	// False - events overflowing the send channel will be dropped.
	BlockOnSend bool

	// BlockOnResponse determines if libhoney should block trying to hand
	// responses back to the caller. If this is true and there is nothing reading
	// from the Responses channel, it will fill up and prevent events from being
	// sent to Honeycomb. Defaults to False - if you don't read from the Responses
	// channel it will be ok.
	BlockOnResponse bool

	// Configuration for the underlying sender. It is safe (and recommended) to
	// leave these values at their defaults. You cannot change these values
	// after calling Init()
	MaxBatchSize         uint          // how many events to collect into a batch before sending
	SendFrequency        time.Duration // how often to send off batches
	MaxConcurrentBatches uint          // how many batches can be inflight simultaneously
	PendingWorkCapacity  uint          // how many events to allow to pile up

}

type Event struct {
	// WriteKey, if set, overrides whatever is found in Config
	WriteKey string
	// Dataset, if set, overrides whatever is found in Config
	Dataset string
	// SampleRate, if set, overrides whatever is found in Config
	SampleRate uint
	// APIHost, if set, overrides whatever is found in Config
	APIHost string
	// Timestamp, if set, specifies the time for this event. If unset, defaults
	// to Now()
	Timestamp time.Time
	// Metadata is a field for you to add in data that will be handed back to you
	// on the Response object read off the Responses channel. It is not sent to
	// Honeycomb with the event.
	Metadata interface{}

	// fieldHolder contains fields (and methods) common to both events and builders
	fieldHolder
}

type Builder struct {
	// WriteKey, if set, overrides whatever is found in Config
	WriteKey string
	// Dataset, if set, overrides whatever is found in Config
	Dataset string
	// SampleRate, if set, overrides whatever is found in Config
	SampleRate uint
	// APIHost, if set, overrides whatever is found in Config
	APIHost string

	// fieldHolder contains fields (and methods) common to both events and builders
	fieldHolder

	// any dynamic fields to apply to each generated event
	dynFields     []dynamicField
	dynFieldsLock sync.Mutex
}

type fieldHolder struct {
	data map[string]interface{}
	lock sync.Mutex
}

// globals for singleton-like behavior
var (
	tx               txClient
	responses        chan Response
	blockOnResponses bool
	sd               *statsd.Client
	globalState      *Builder
)

type dynamicField struct {
	name string
	fn   func() interface{}
}

// initialize a default config to protect ourselves against using unitialized
// values if someone forgets to run Init(). It's fine if things don't work
// without running Init; it's not fine if they panic.
func init() {
	// initialize global statsd client as mute to provide a working default
	sd, _ = statsd.New(statsd.Mute(true))
	globalState = &Builder{
		SampleRate: 1,
		dynFields:  make([]dynamicField, 0, 0),
	}
	globalState.data = make(map[string]interface{})
}

// Init must be called once on app initialization. All fields in the Config
// struct are optional. If WriteKey and DataSet are absent, they must be
// specified later, either on a builder or an event. WriteKey, Dataset,
// SampleRate, and APIHost can all be overridden on a per-builder or per-event
// basis.
//
// Make sure to call Close() to flush transmisison buffers.
func Init(config Config) error {
	// Default sample rate should be 1. 0 is invalid.
	if config.SampleRate == 0 {
		config.SampleRate = defaultSampleRate
	}
	if config.APIHost == "" {
		config.APIHost = defaultAPIHost
	}

	sd, _ = statsd.New(statsd.Prefix("libhoney"))

	responses = make(chan Response, config.PendingWorkCapacity*2)

	// spin up the global transmission
	tx = &txDefaultClient{
		maxBatchSize:         config.MaxBatchSize,
		batchTimeout:         config.SendFrequency,
		maxConcurrentBatches: config.MaxConcurrentBatches,
		pendingWorkCapacity:  config.PendingWorkCapacity,
		blockOnSend:          config.BlockOnSend,
	}

	if err := tx.Start(); err != nil {
		return err
	}

	globalState = &Builder{
		WriteKey:   config.WriteKey,
		Dataset:    config.Dataset,
		SampleRate: config.SampleRate,
		APIHost:    config.APIHost,
		dynFields:  make([]dynamicField, 0, 0),
	}
	globalState.data = make(map[string]interface{})

	return nil
}

// Close waits for all in-flight messages to be sent. You should
// call Close() before app termination.
func Close() {
	tx.Stop()
	close(responses)
}

// SendNow is a shortcut to create an event, add data, and send the event.
func SendNow(data interface{}) error {
	ev := NewEvent()
	if err := ev.Add(data); err != nil {
		return err
	}
	if err := ev.Send(); err != nil {
		return err
	}
	return nil
}

// Responses returns the channel from which the caller can read the responses
// to sent events
func Responses() chan Response {
	return responses
}

// AddDynamicField takes a field name and a function that will generate values
// for that metric. The function is called once every time a NewEvent() is
// created and added as a field (with name as the key) to the newly created
// event.
func AddDynamicField(name string, fn func() interface{}) error {
	return globalState.AddDynamicField(name, fn)
}

// AddField adds a Field to the global scope. This metric will be inherited by
// all builders and events.
func AddField(name string, val interface{}) {
	globalState.AddField(name, val)
}

// Add adds its data to the global scope. It adds all fields in a struct or all
// keys in a map as individual Fields. These metrics will be inherited by all
// builders and events.
func Add(data interface{}) error {
	return globalState.Add(data)
}

// Creates a new event prepopulated with any Fields present in the global
// scope.
func NewEvent() *Event {
	return globalState.NewEvent()
}

// AddField adds an individual metric to the event or builder on which it is
// called.
func (f *fieldHolder) AddField(key string, val interface{}) {
	f.lock.Lock()
	defer f.lock.Unlock()
	// run a sanity check on data, transparently drop if it fails.
	if validateData(val) {
		f.data[key] = val
	}
}

// validateData runs some checks on the data and returns false if it's bad data
// and should be skipped
func validateData(val interface{}) bool {
	if val == nil {
		return false
	}
	// if we can't json encode the value, we should skip it.
	// TODO this is probably slow. Decide whether it's unacceptably slow.
	_, err := json.Marshal(val)
	if err != nil {
		return false
	}
	return true
}

func validateValue(val reflect.Value) bool {
	if val.Type().Kind() == reflect.Chan {
		return false
	}
	kind := val.Type().Kind()
	for _, ptrKind := range ptrKinds {
		if kind == ptrKind && val.IsNil() {
			return false
		}
	}
	if validateData(val.Interface()) == false {
		return false
	}
	return true
}

// Add adds a complex data type to the event or builder on which it's called.
// For structs, it adds each exported field. For maps, it adds each key/value.
// Add will error on all other types.
func (f *fieldHolder) Add(data interface{}) error {
	switch reflect.TypeOf(data).Kind() {
	case reflect.Struct:
		return f.addStruct(data)
	case reflect.Map:
		return f.addMap(data)
	case reflect.Ptr:
		return f.Add(reflect.ValueOf(data).Elem().Interface())
	}
	return fmt.Errorf(
		"Couldn't add type %s content %+v",
		reflect.TypeOf(data).Kind(), data,
	)
}

func (f *fieldHolder) addStruct(s interface{}) error {
	f.lock.Lock()
	defer f.lock.Unlock()

	// TODO should we handle embedded structs differently from other deep structs?
	sType := reflect.TypeOf(s)
	sVal := reflect.ValueOf(s)
	// Iterate through the fields, adding each.
	for i := 0; i < sType.NumField(); i++ {
		fieldInfo := sType.Field(i)
		if fieldInfo.PkgPath != "" {
			// skipping unexported field in the struct
			continue
		}

		var fName string
		fTag := fieldInfo.Tag.Get("json")
		if fTag != "" {
			if fTag == "-" {
				// skip this field
				continue
			}
			// slice off options
			if idx := strings.Index(fTag, ","); idx != -1 {
				fTag = fTag[:idx]
			}
			fName = fTag
		} else {
			fName = fieldInfo.Name
		}

		if validateValue(sVal.Field(i)) {
			f.data[fName] = sVal.Field(i).Interface()
		}
	}
	return nil
}

func (f *fieldHolder) addMap(m interface{}) error {
	f.lock.Lock()
	defer f.lock.Unlock()

	mVal := reflect.ValueOf(m)
	mKeys := mVal.MapKeys()
	for _, key := range mKeys {
		// get a string representation of key
		var keyStr string
		switch key.Type().Kind() {
		case reflect.String:
			keyStr = key.String()
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32,
			reflect.Int64, reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32,
			reflect.Uint64, reflect.Float32, reflect.Float64, reflect.Complex64,
			reflect.Complex128:
			keyStr = fmt.Sprintf("%v", key.Interface())
		default:
			return fmt.Errorf("failed to add map: key type %s unaccepted", key.Type().Kind())
		}
		if validateValue(mVal.MapIndex(key)) {
			f.data[keyStr] = mVal.MapIndex(key).Interface()
		}
	}
	return nil
}

// AddFunc takes a function and runs it repeatedly, adding the return values
// as fields.
// The function should return error when it has exhausted its values
func (f *fieldHolder) AddFunc(fn func() (string, interface{}, error)) error {
	for {
		key, rawVal, err := fn()
		if err != nil {
			// fn is done giving us data
			break
		}
		f.AddField(key, rawVal)
	}
	return nil
}

// Send dispatches the event to be sent to Honeycomb.
//
// If you have sampling enabled
// (i.e. SampleRate >1), Send will only actually transmit data with a
// probability of 1/SampleRate. No error is returned whether or not traffic
// is sampled, however, the Response sent down the response channel will
// indicate the event was sampled in the errors Err field.
//
// Send inherits the values of required fields from Config. If any required
// fields are specified in neither Config nor the Event, Send will return an
// error.  Required fields are APIHost, WriteKey, and Dataset. Values specified
// in an Event override Config.
func (e *Event) Send() error {
	if shouldDrop(e.SampleRate) {
		sd.Increment("sampled")
		sendDroppedResponse(e, "event dropped due to sampling")
		return nil
	}
	if len(e.data) == 0 {
		return errors.New("No metrics added to event. Won't send empty event.")
	}
	if e.APIHost == "" {
		return errors.New("No APIHost for Honeycomb. Can't send to the Great Unknown.")
	}
	if e.WriteKey == "" {
		return errors.New("No WriteKey specified. Can't send event.")
	}
	if e.Dataset == "" {
		return errors.New("No Dataset for Honeycomb. Can't send datasetless.")
	}

	tx.Add(e)
	return nil
}

// sendResponse sends a dropped event response down the response channel
func sendDroppedResponse(e *Event, message string) {
	r := Response{
		Err:      errors.New(message),
		Metadata: e.Metadata,
	}
	if blockOnResponses {
		responses <- r
	} else {
		select {
		case responses <- r:
		default:
		}
	}
}

// returns true if the sample should be dropped
func shouldDrop(rate uint) bool {
	return rand.Intn(int(rate)) != 0
}

// returns true if the first character of the string is lowercase
func isFirstLower(s string) bool {
	return false
}

// NewBuilder creates a new event builder. The builder inherits any
// Dynamic or Static Fields present in the global scope.
func NewBuilder() *Builder {
	return globalState.Clone()
}

// AddDynamicField adds a dynamic field to the builder. Any events
// created from this builder will get this metric added.
func (b *Builder) AddDynamicField(name string, fn func() interface{}) error {
	b.dynFieldsLock.Lock()
	defer b.dynFieldsLock.Unlock()
	dynFn := dynamicField{
		name: name,
		fn:   fn,
	}
	b.dynFields = append(b.dynFields, dynFn)
	return nil
}

// SendNow is a shortcut to create an event from this builder, add data, and
// send the event.
func (b *Builder) SendNow(data interface{}) error {
	ev := b.NewEvent()
	if err := ev.Add(data); err != nil {
		return err
	}
	if err := ev.Send(); err != nil {
		return err
	}
	return nil
}

// NewEvent creates a new Event prepopulated with fields, dynamic
// field values, and configuration inherited from the builder.
func (b *Builder) NewEvent() *Event {
	e := &Event{
		WriteKey:   b.WriteKey,
		Dataset:    b.Dataset,
		SampleRate: b.SampleRate,
		APIHost:    b.APIHost,
		Timestamp:  time.Now(),
	}
	e.data = make(map[string]interface{})

	// copy static metrics (everything's been serialized so flat copy is OK)
	for k, v := range b.data {
		e.data[k] = v
	}
	// create dynamic metrics
	for _, dynField := range b.dynFields {
		e.AddField(dynField.name, dynField.fn())
	}
	return e
}

// Clone creates a new builder that inherits all traits of this builder and
// creates its own scope in which to add additional static and dynamic fields.
func (b *Builder) Clone() *Builder {
	newB := &Builder{
		WriteKey:   b.WriteKey,
		Dataset:    b.Dataset,
		SampleRate: b.SampleRate,
		APIHost:    b.APIHost,
		dynFields:  make([]dynamicField, 0, len(b.dynFields)),
	}
	newB.data = make(map[string]interface{})
	// copy static metrics (everything's been serialized so flat copy is OK)
	for k, v := range b.data {
		newB.data[k] = v
	}
	// copy dynamic metric generators
	for _, dynFd := range b.dynFields {
		newB.dynFields = append(newB.dynFields, dynFd)
	}
	return newB
}
