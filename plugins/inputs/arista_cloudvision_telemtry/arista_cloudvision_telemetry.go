//go:generate ../../../tools/readme_config_includer/generator
package arista_cloudvision_telemtry

import (
	"bytes"
	"context"
	"crypto/tls"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"math"
	"net/http"
	"path"
	"strings"
	"sync"
	"time"

	gnmiLib "github.com/openconfig/gnmi/proto/gnmi"
	"golang.org/x/oauth2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/oauth"

	"github.com/influxdata/telegraf/metric"
	internaltls "github.com/influxdata/telegraf/plugins/common/tls"

	"github.com/influxdata/telegraf"
	"github.com/influxdata/telegraf/config"
	"github.com/influxdata/telegraf/plugins/inputs"
	jsonparser "github.com/influxdata/telegraf/plugins/parsers/json"
)

// DO NOT REMOVE THE NEXT TWO LINES! This is required to embed the sampleConfig data.
//go:embed sample.conf
var sampleConfig string

// Cloudvision struct
type CVP struct {
	// CVP information
	Cvpaddress    string         `toml:"addresses"`
	Subscriptions []Subscription `toml:"subscription"`

	Encoding    string
	Origin      string //origin openconfig is supported today only.
	Prefix      string
	UpdatesOnly bool `toml:"updates_only"`

	Cvptoken string `toml:"cvptoken"` // JSON web toke for ClouVision

	// Redial
	Redial config.Duration

	// GRPC TLS settings
	EnableTLS bool `toml:"enable_tls"`
	internaltls.ClientConfig

	// Internal state
	internalAliases map[string]string
	acc             telegraf.Accumulator
	cancel          context.CancelFunc
	wg              sync.WaitGroup
	// Lookup/device+name/key/value
	lookup      map[string]map[string]map[string]interface{}
	lookupMutex sync.Mutex

	// Device target -> name
	devices map[string]string

	Log telegraf.Logger
}

type Subscription struct {
	Name   string
	Origin string //origin openconfig is supported today only.
	Path   string

	// Subscription mode and interval
	SubscriptionMode string          `toml:"subscription_mode"`
	SampleInterval   config.Duration `toml:"sample_interval"`

	// Duplicate suppression
	SuppressRedundant bool            `toml:"suppress_redundant"`
	HeartbeatInterval config.Duration `toml:"heartbeat_interval"`
}

// Struct for cloudvision API to return device data.
type CvPDevices struct {
	Result struct {
		Value struct {
			Key struct {
				DeviceID string `json:"deviceId"`
			} `json:"key"`
			SoftwareVersion    string    `json:"softwareVersion"`
			ModelName          string    `json:"modelName"`
			HardwareRevision   string    `json:"hardwareRevision"`
			Fqdn               string    `json:"fqdn"`
			Hostname           string    `json:"hostname"`
			DomainName         string    `json:"domainName"`
			SystemMacAddress   string    `json:"systemMacAddress"`
			BootTime           time.Time `json:"bootTime"`
			StreamingStatus    string    `json:"streamingStatus"`
			ExtendedAttributes struct {
				FeatureEnabled struct {
					Danz bool `json:"Danz"`
					Mlag bool `json:"Mlag"`
				} `json:"featureEnabled"`
			} `json:"extendedAttributes"`
		} `json:"value"`
		Time time.Time `json:"time"`
		Type string    `json:"type"`
	} `json:"result"`
}

func (*CVP) SampleConfig() string {
	return sampleConfig
}

// Start the CVP gNMI telemetry service
func (c *CVP) Start(acc telegraf.Accumulator) error {
	var err error
	var ctx context.Context
	var tlscfg *tls.Config
	c.acc = acc
	ctx, c.cancel = context.WithCancel(context.Background())
	c.lookupMutex.Lock()
	c.lookup = make(map[string]map[string]map[string]interface{})
	c.devices = make(map[string]string)
	c.lookupMutex.Unlock()

	// Parse TLS config
	if c.EnableTLS {
		if tlscfg, err = c.ClientConfig.TLSConfig(); err != nil {
			return err
		}
	}

	if c.Cvptoken != "" {
		tlscfg = &tls.Config{
			Renegotiation:      tls.RenegotiateNever,
			InsecureSkipVerify: true,
		}
	}

	if time.Duration(c.Redial).Nanoseconds() <= 0 {
		return fmt.Errorf("redial duration must be positive")
	}

	go func() {
		for {
			//Create a map of devices.  This should read device:target.  The target is the same as the serial number to CVP.
			var cvdevs []string

			for cvpdevice, devicetarget := range c.CvpDevices() {
				if _, ok := c.devices[devicetarget]; !ok {
					c.devices[devicetarget] = cvpdevice
					cvdevs = append(cvdevs, devicetarget)
				}
			}
			if len(cvdevs) != 0 {
				// Validate configuration
				var request []*gnmiLib.SubscribeRequest
				if request, err = c.newSubscribeRequest(cvdevs); err != nil {
					fmt.Println(err)
				}

				// Start the request for gNMI interface on CVP.
				c.wg.Add(len(cvdevs))
				CvpAddr := c.Cvpaddress
				for i := range request {
					// Loop through the requests for targets.
					thisReq := make([]*gnmiLib.SubscribeRequest, 1)
					copy(thisReq, (request[i:(i + 1)]))
					go func(CvpAddr string) {
						defer c.wg.Done()
						for ctx.Err() == nil {
							if err := c.subscribeGNMI(ctx, CvpAddr, tlscfg, thisReq[0]); err != nil && ctx.Err() == nil {
								acc.AddError(err)
							}

							select {
							case <-ctx.Done():
							case <-time.After(time.Duration(c.Redial)):
							}
						}
					}(CvpAddr)
				}
			}
			time.Sleep(1000 * 1000 * 1000 * 15)
		}
	}()
	return nil
}

// Method to return all devices which are streaming so we can then use their targets as the gNMI target.
func (c *CVP) CvpDevices() map[string]string {
	var bearer = "Bearer " + c.Cvptoken
	//Connect to CVP resource api
	req, err := http.NewRequest("GET", "https://"+c.Cvpaddress+"/api/resources/inventory/v1/Device/all", nil)
	if err != nil {
		c.Log.Error("Cannot connect to CVP", err)
	}
	req.Header.Add("Authorization", bearer)
	req.Header.Add("Accept", "application/json")

	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}

	client := &http.Client{Transport: tr}

	resp, err := client.Do(req)
	if err != nil {
		c.Log.Error("Cannot connect to CVP", err)
	}
	defer resp.Body.Close()

	responseData, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		c.Log.Error("Cannot marshall data", err)
	}

	f := strings.Split(string(responseData), "\n")
	//Create a map for devices
	devs := map[string]string{}
	//Loop through and add devices to devs map that are currently streaming.
	for _, i := range f {
		var dev CvPDevices
		err = json.Unmarshal([]byte(i), &dev)
		if err != nil {
			c.Log.Debugf("Cannot marshall HTTP Connection to CVP")
		}
		if dev.Result.Value.StreamingStatus == "STREAMING_STATUS_ACTIVE" {
			devs[dev.Result.Value.Fqdn] = dev.Result.Value.Key.DeviceID
		}
	}
	//Return devices.
	return devs
}

//ParsePath from XPath-like string to gNMI path structure
func parsePath(origin string, pathToParse string, target []string) ([]*gnmiLib.Path, error) {
	var err error
	var gnmilibsslice []*gnmiLib.Path
	//Might need to add some concurrency here?
	for _, targets := range target {
		gnmiPath := gnmiLib.Path{Origin: origin, Target: targets}

		if len(pathToParse) > 0 && pathToParse[0] != '/' {
			return nil, fmt.Errorf("path does not start with a '/': %s", pathToParse)
		}

		elem := &gnmiLib.PathElem{}
		start, name, value, end := 0, -1, -1, -1

		pathToParse = pathToParse + "/"

		for i := 0; i < len(pathToParse); i++ {
			if pathToParse[i] == '[' {
				if name >= 0 {
					break
				}
				if end < 0 {
					end = i
					elem.Key = make(map[string]string)
				}
				name = i + 1
			} else if pathToParse[i] == '=' {
				if name <= 0 || value >= 0 {
					break
				}
				value = i + 1
			} else if pathToParse[i] == ']' {
				if name <= 0 || value <= name {
					break
				}
				elem.Key[pathToParse[name:value-1]] = strings.Trim(pathToParse[value:i], "'\"")
				name, value = -1, -1
			} else if pathToParse[i] == '/' {
				if name < 0 {
					if end < 0 {
						end = i
					}

					if end > start {
						elem.Name = pathToParse[start:end]
						gnmiPath.Elem = append(gnmiPath.Elem, elem)
						gnmiPath.Element = append(gnmiPath.Element, pathToParse[start:i])
					}

					start, name, value, end = i+1, -1, -1, -1
					elem = &gnmiLib.PathElem{}
				}
			}
		}

		if name >= 0 || value >= 0 {
			err = fmt.Errorf("Invalid gNMI path: %s", pathToParse)
		}

		if err != nil {
			return nil, err
		}
		gnmilibsslice = append(gnmilibsslice, &gnmiPath)
	}
	return gnmilibsslice, nil
}

func (c *CVP) newSubscribeRequest(targets []string) ([]*gnmiLib.SubscribeRequest, error) {
	// Create subscription objects
	targetLen := len(targets)
	subLen := len(c.Subscriptions)

	// Layout as [sub1Host1, sub2Host1, sub1Host2, sub2Host2]
	subscriptions := make([]*gnmiLib.Subscription, subLen*targetLen)

	for i, subscription := range c.Subscriptions {
		gnmiPath, err := parsePath(subscription.Origin, subscription.Path, targets)
		if err != nil {
			return nil, err
		}
		mode, ok := gnmiLib.SubscriptionMode_value[strings.ToUpper(subscription.SubscriptionMode)]
		if !ok {
			return nil, fmt.Errorf("invalid subscription mode %s", subscription.SubscriptionMode)
		}
		for j, path := range gnmiPath {
			subscriptions[(j*subLen)+i] = &gnmiLib.Subscription{
				Path:              path,
				Mode:              gnmiLib.SubscriptionMode(mode),
				SampleInterval:    uint64(time.Duration(subscription.SampleInterval).Nanoseconds()),
				SuppressRedundant: subscription.SuppressRedundant,
				HeartbeatInterval: uint64(time.Duration(subscription.HeartbeatInterval).Nanoseconds()),
			}
		}
	}

	// Construct subscribe request
	gnmiPath, err := parsePath(c.Origin, c.Prefix, targets)
	if err != nil {
		return nil, err
	}

	if c.Encoding != "proto" && c.Encoding != "json" && c.Encoding != "json_ietf" && c.Encoding != "bytes" {
		return nil, fmt.Errorf("unsupported encoding %s", c.Encoding)
	}

	var subSlice []*gnmiLib.SubscribeRequest

	for j, path := range gnmiPath {
		req := gnmiLib.SubscribeRequest{
			Request: &gnmiLib.SubscribeRequest_Subscribe{
				Subscribe: &gnmiLib.SubscriptionList{
					Prefix:   path,
					Mode:     gnmiLib.SubscriptionList_STREAM,
					Encoding: gnmiLib.Encoding(gnmiLib.Encoding_value[strings.ToUpper(c.Encoding)]),
					// Remember the layout of subscriptions is [sub1Host1, sub2Host1, sub1Host2, sub2Host2]
					Subscription: subscriptions[j*subLen : (j+1)*subLen],
					UpdatesOnly:  c.UpdatesOnly,
				},
			},
		}
		subSlice = append(subSlice, &req)
	}
	return subSlice, nil
}

// SubscribeGNMI and extract telemetry data
func (c *CVP) subscribeGNMI(ctx context.Context, address string, tlscfg *tls.Config, request *gnmiLib.SubscribeRequest) error {
	// Create a slice of grpc options for multiple different options.
	var options []grpc.DialOption
	if len(c.Cvptoken) > 0 {
		options = append(options, grpc.WithPerRPCCredentials((oauth.NewOauthAccess(&oauth2.Token{
			AccessToken: c.Cvptoken,
		}))))
	}
	if tlscfg != nil {
		options = append(options, grpc.WithTransportCredentials(credentials.NewTLS(tlscfg)))
	}

	client, err := grpc.DialContext(ctx, address, options...)
	if err != nil {
		return fmt.Errorf("failed to dial: %v", err)
	}
	defer client.Close()

	subscribeClient, err := gnmiLib.NewGNMIClient(client).Subscribe(ctx)
	if err != nil {
		return fmt.Errorf("failed to setup subscription: %v", err)
	}

	if err = subscribeClient.Send(request); err != nil {
		// If io.EOF is returned, the stream may have ended and stream status
		// can be determined by calling Recv.
		if err != io.EOF {
			return fmt.Errorf("failed to send subscription request: %v", err)
		}
	}
	c.Log.Info("Connection to gNMI device established for device target ")

	defer c.Log.Debugf("Connection to gNMI device %s closed", address)
	for ctx.Err() == nil {
		var reply *gnmiLib.SubscribeResponse
		if reply, err = subscribeClient.Recv(); err != nil {
			if err != io.EOF && ctx.Err() == nil {
				return fmt.Errorf("aborted gNMI subscription: %v", err)
			}
			break
		}

		c.handleSubscribeResponse(address, reply)
	}
	return nil
}

func (c *CVP) handleSubscribeResponse(address string, reply *gnmiLib.SubscribeResponse) {
	switch response := reply.Response.(type) {
	case *gnmiLib.SubscribeResponse_Update:
		c.handleSubscribeResponseUpdate(address, response)
	case *gnmiLib.SubscribeResponse_Error:
		c.Log.Errorf("Subscribe error (%d), %q", response.Error.Code, response.Error.Message)
	}
}

// Handle SubscribeResponse_Update message from gNMI and parse contained telemetry data
func (c *CVP) handleSubscribeResponseUpdate(address string, response *gnmiLib.SubscribeResponse_Update) {
	var prefix, prefixAliasPath string
	grouper := metric.NewSeriesGrouper()
	timestamp := time.Unix(0, response.Update.Timestamp)
	prefixTags := make(map[string]string)
	if response.Update.Prefix != nil {
		var err error
		if prefix, prefixAliasPath, err = c.handlePath(response.Update.Prefix, prefixTags, ""); err != nil {
			c.Log.Errorf("handling path %q failed: %v", response.Update.Prefix, err)
		}
	}
	prefixTags["host-id"] = response.Update.Prefix.Target
	prefixTags["host"] = c.devices[prefixTags["host-id"]]

	// Parse individual Update message and create measurements
	var name, lastAliasPath string
	for _, update := range response.Update.Update {
		// Prepare tags from prefix
		tags := make(map[string]string, len(prefixTags))
		for key, val := range prefixTags {
			tags[key] = val
		}
		aliasPath, fields := c.handleTelemetryField(update, tags, prefix)

		// Inherent valid alias from prefix parsing
		if len(prefixAliasPath) > 0 && len(aliasPath) == 0 {
			aliasPath = prefixAliasPath
		}

		// Lookup alias if alias-path has changed
		if aliasPath != lastAliasPath {
			name = prefix
			if alias, ok := c.internalAliases[aliasPath]; ok {
				name = alias
			} else {
				c.Log.Debugf("No measurement alias for gNMI path: %s", name)
			}
		}

		// Update tag lookups and discard rest of update
		subscriptionKey := tags["source"] + "/" + tags["name"]
		c.lookupMutex.Lock()
		if _, ok := c.lookup[name]; ok {
			// We are subscribed to this, so add the fields to the lookup-table
			if _, ok := c.lookup[name][subscriptionKey]; !ok {
				c.lookup[name][subscriptionKey] = make(map[string]interface{})
			}
			for k, v := range fields {
				c.lookup[name][subscriptionKey][path.Base(k)] = v
			}
			c.lookupMutex.Unlock()
			// Do not process the data further as we only subscribed here for the lookup table
			continue
		}

		// Apply lookups if present
		for subscriptionName, values := range c.lookup {
			if annotations, ok := values[subscriptionKey]; ok {
				for k, v := range annotations {
					tags[subscriptionName+"/"+k] = fmt.Sprint(v)
				}
			}
		}
		c.lookupMutex.Unlock()

		// Group metrics
		for k, v := range fields {
			key := k
			if len(aliasPath) < len(key) && len(aliasPath) != 0 {
				// This may not be an exact prefix, due to naming style
				// conversion on the key.
				key = key[len(aliasPath)+1:]
			} else if len(aliasPath) >= len(key) {
				// Otherwise use the last path element as the field key.
				key = path.Base(key)

				// If there are no elements skip the item; this would be an
				// invalid message.
				key = strings.TrimLeft(key, "/.")
				if key == "" {
					c.Log.Errorf("invalid empty path: %q", k)
					continue
				}
			}

			if err := grouper.Add(key, tags, timestamp, key, v); err != nil {
				c.Log.Errorf("cannot add to grouper: %v", err)
			}
		}

		lastAliasPath = aliasPath
	}

	// Add grouped measurements
	for _, metricToAdd := range grouper.Metrics() {
		c.acc.AddMetric(metricToAdd)
	}
}

// HandleTelemetryField and add it to a measurement
func (c *CVP) handleTelemetryField(update *gnmiLib.Update, tags map[string]string, prefix string) (string, map[string]interface{}) {
	gpath, aliasPath, err := c.handlePath(update.Path, tags, prefix)
	if err != nil {
		c.Log.Errorf("handling path %q failed: %v", update.Path, err)
	}

	var value interface{}
	var jsondata []byte

	// Make sure a value is actually set
	if update.Val == nil || update.Val.Value == nil {
		c.Log.Infof("Discarded empty or legacy type value with path: %q", gpath)
		return aliasPath, nil
	}

	switch val := update.Val.Value.(type) {
	case *gnmiLib.TypedValue_AsciiVal:
		value = val.AsciiVal
	case *gnmiLib.TypedValue_BoolVal:
		value = val.BoolVal
	case *gnmiLib.TypedValue_BytesVal:
		value = val.BytesVal
	case *gnmiLib.TypedValue_DecimalVal:
		value = float64(val.DecimalVal.Digits) / math.Pow(10, float64(val.DecimalVal.Precision))
	case *gnmiLib.TypedValue_FloatVal:
		value = val.FloatVal
	case *gnmiLib.TypedValue_IntVal:
		value = val.IntVal
	case *gnmiLib.TypedValue_StringVal:
		value = val.StringVal
	case *gnmiLib.TypedValue_UintVal:
		value = val.UintVal
	case *gnmiLib.TypedValue_JsonIetfVal:
		jsondata = val.JsonIetfVal
	case *gnmiLib.TypedValue_JsonVal:
		jsondata = val.JsonVal
	}

	name := strings.ReplaceAll(gpath, "-", "_")
	fields := make(map[string]interface{})
	if value != nil {
		fields[name] = value
	} else if jsondata != nil {
		if err := json.Unmarshal(jsondata, &value); err != nil {
			c.acc.AddError(fmt.Errorf("failed to parse JSON value: %v", err))
		} else {
			flattener := jsonparser.JSONFlattener{Fields: fields}
			if err := flattener.FullFlattenJSON(name, value, true, true); err != nil {
				c.acc.AddError(fmt.Errorf("failed to flatten JSON: %v", err))
			}
		}
	}
	return aliasPath, fields
}

// Parse path to path-buffer and tag-field
func (c *CVP) handlePath(gnmiPath *gnmiLib.Path, tags map[string]string, prefix string) (pathBuffer string, aliasPath string, err error) {
	builder := bytes.NewBufferString(prefix)

	// Prefix with origin
	if len(gnmiPath.Origin) > 0 {
		if _, err := builder.WriteString(gnmiPath.Origin); err != nil {
			return "", "", err
		}
		if _, err := builder.WriteRune(':'); err != nil {
			return "", "", err
		}
	}

	// Parse generic keys from prefix
	for _, elem := range gnmiPath.Elem {
		if len(elem.Name) > 0 {
			if _, err := builder.WriteRune('/'); err != nil {
				return "", "", err
			}
			if _, err := builder.WriteString(elem.Name); err != nil {
				return "", "", err
			}
		}
		name := builder.String()

		if _, exists := c.internalAliases[name]; exists {
			aliasPath = name
		}

		if tags != nil {
			for key, val := range elem.Key {
				key = strings.ReplaceAll(key, "-", "_")

				// Use short-form of key if possible
				if _, exists := tags[key]; exists {
					tags[name+"/"+key] = val
				} else {
					tags[key] = val
				}
			}
		}
	}

	return builder.String(), aliasPath, nil
}

// Stop listener and cleanup
func (c *CVP) Stop() {
	c.cancel()
	c.wg.Wait()
}

func New() telegraf.Input {
	return &CVP{
		Encoding: "proto",
		Redial:   config.Duration(10 * time.Second),
	}
}

// Gather plugin measurements (unused)
func (c *CVP) Gather(_ telegraf.Accumulator) error {
	return nil
}

func init() {
	inputs.Add("arista_cloudvision_telemtry", New)
}
