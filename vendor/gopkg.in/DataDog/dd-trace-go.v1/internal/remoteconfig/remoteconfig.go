// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2022 Datadog, Inc.

package remoteconfig

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"time"

	"gopkg.in/DataDog/dd-trace-go.v1/internal/log"

	rc "github.com/DataDog/datadog-agent/pkg/remoteconfig/state"
)

// Callback represents a function that can process a remote config update.
// A Callback function can be registered to a remote config client to automatically
// react upon receiving updates. This function returns the configuration processing status
// for each config file received through the update.
type Callback func(updates map[string]ProductUpdate) map[string]rc.ApplyStatus

// ProductCallback is like Callback but for a specific product.
type ProductCallback func(update ProductUpdate) map[string]rc.ApplyStatus

// Capability represents a bit index to be set in clientData.Capabilites in order to register a client
// for a specific capability
type Capability uint

const (
	_ Capability = iota
	// ASMActivation represents the capability to activate ASM through remote configuration
	ASMActivation
	// ASMIPBlocking represents the capability for ASM to block requests based on user IP
	ASMIPBlocking
	// ASMDDRules represents the capability to update the rules used by the ASM WAF for threat detection
	ASMDDRules
	// ASMExclusions represents the capability for ASM to exclude traffic from its protections
	ASMExclusions
	// ASMRequestBlocking represents the capability for ASM to block requests based on the HTTP request related WAF addresses
	ASMRequestBlocking
	// ASMResponseBlocking represents the capability for ASM to block requests based on the HTTP response related WAF addresses
	ASMResponseBlocking
	// ASMUserBlocking represents the capability for ASM to block requests based on user ID
	ASMUserBlocking
	// ASMCustomRules represents the capability for ASM to receive and use user-defined security rules
	ASMCustomRules
	// ASMCustomBlockingResponse represents the capability for ASM to receive and use user-defined blocking responses
	ASMCustomBlockingResponse
	// ASMTrustedIPs represents Trusted IPs through the ASM product
	ASMTrustedIPs
	// ASMApiSecuritySampleRate represents API Security sampling rate
	ASMApiSecuritySampleRate
	// APMTracingSampleRate represents the rate at which to sample traces from APM client libraries
	APMTracingSampleRate
	// APMTracingLogsInjection enables APM client libraries to inject trace ids into log records
	APMTracingLogsInjection
	// APMTracingHTTPHeaderTags enables APM client libraries to tag http header values to http server or client spans
	APMTracingHTTPHeaderTags
	// APMTracingCustomTags enables APM client to set custom tags on all spans
	APMTracingCustomTags
)

// APMTracingEnabled enables APM tracing
const APMTracingEnabled Capability = 19

// ErrClientNotStarted is returned when the remote config client is not started.
var ErrClientNotStarted = errors.New("remote config client not started")

// ProductUpdate represents an update for a specific product.
// It is a map of file path to raw file content
type ProductUpdate map[string][]byte

// A Client interacts with an Agent to update and track the state of remote
// configuration
type Client struct {
	sync.RWMutex
	ClientConfig

	clientID   string
	endpoint   string
	repository *rc.Repository
	stop       chan struct{}

	// When acquiring several locks and using defer to release them, make sure to acquire the locks in the following order:
	callbacks               []Callback
	_callbacksMu            sync.RWMutex
	products                map[string]struct{}
	productsMu              sync.RWMutex
	productsWithCallbacks   map[string]ProductCallback
	productsWithCallbacksMu sync.RWMutex
	capabilities            map[Capability]struct{}
	capabilitiesMu          sync.RWMutex

	lastError error
}

// client is a RC client singleton that can be accessed by multiple products (tracing, ASM, profiling etc.).
// Using a single RC client instance in the tracer is a requirement for remote configuration.
var client *Client

var (
	startOnce sync.Once
	stopOnce  sync.Once
)

// newClient creates a new remoteconfig Client
func newClient(config ClientConfig) (*Client, error) {
	repo, err := rc.NewUnverifiedRepository()
	if err != nil {
		return nil, err
	}
	if config.HTTP == nil {
		config.HTTP = DefaultClientConfig().HTTP
	}

	return &Client{
		ClientConfig:          config,
		clientID:              generateID(),
		endpoint:              fmt.Sprintf("%s/v0.7/config", config.AgentURL),
		repository:            repo,
		stop:                  make(chan struct{}),
		lastError:             nil,
		callbacks:             []Callback{},
		capabilities:          map[Capability]struct{}{},
		products:              map[string]struct{}{},
		productsWithCallbacks: make(map[string]ProductCallback),
	}, nil
}

// Start starts the client's update poll loop in a fresh goroutine.
// Noop if the client has already started.
func Start(config ClientConfig) error {
	var err error
	startOnce.Do(func() {
		client, err = newClient(config)
		if err != nil {
			return
		}
		go func() {
			ticker := time.NewTicker(client.PollInterval)
			defer ticker.Stop()

			for {
				select {
				case <-client.stop:
					close(client.stop)
					return
				case <-ticker.C:
					client.Lock()
					client.updateState()
					client.Unlock()
				}
			}
		}()
	})
	return err
}

// Stop stops the client's update poll loop.
// Noop if the client has already been stopped.
// The remote config client is supposed to have the same lifecycle as the tracer.
// It can't be restarted after a call to Stop() unless explicitly calling Reset().
func Stop() {
	if client == nil {
		// In case Stop() is called before Start()
		return
	}
	stopOnce.Do(func() {
		log.Debug("remoteconfig: gracefully stopping the client")
		client.stop <- struct{}{}
		select {
		case <-client.stop:
			log.Debug("remoteconfig: client stopped successfully")
		case <-time.After(time.Second):
			log.Debug("remoteconfig: client stopping timeout")
		}
	})
}

// Reset destroys the client instance.
// To be used only in tests to reset the state of the client.
func Reset() {
	client = nil
	startOnce = sync.Once{}
	stopOnce = sync.Once{}
}

func (c *Client) updateState() {
	data, err := c.newUpdateRequest()
	if err != nil {
		log.Error("remoteconfig: unexpected error while creating a new update request payload: %v", err)
		return
	}

	req, err := http.NewRequest(http.MethodGet, c.endpoint, &data)
	if err != nil {
		log.Error("remoteconfig: unexpected error while creating a new http request: %v", err)
		return
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		log.Debug("remoteconfig: http request error: %v", err)
		return
	}
	// Flush and close the response body when returning (cf. https://pkg.go.dev/net/http#Client.Do)
	defer func() {
		io.ReadAll(resp.Body)
		resp.Body.Close()
	}()

	if sc := resp.StatusCode; sc != http.StatusOK {
		log.Debug("remoteconfig: http request error: response status code is not 200 (OK) but %s", http.StatusText(sc))
		return
	}

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Error("remoteconfig: http request error: could not read the response body: %v", err)
		return
	}

	if body := string(respBody); body == `{}` || body == `null` {
		return
	}

	var update clientGetConfigsResponse
	if err := json.Unmarshal(respBody, &update); err != nil {
		log.Error("remoteconfig: http request error: could not parse the json response body: %v", err)
		return
	}

	c.lastError = c.applyUpdate(&update)
}

// Subscribe registers a product and its callback to be invoked when the client receives configuration updates.
// Subscribe should be preferred over RegisterProduct and RegisterCallback if your callback only handles a single product.
func Subscribe(product string, callback ProductCallback, capabilities ...Capability) error {
	if client == nil {
		return ErrClientNotStarted
	}
	client.productsMu.RLock()
	defer client.productsMu.RUnlock()
	if _, found := client.products[product]; found {
		return fmt.Errorf("product %s already registered via RegisterProduct", product)
	}

	client.productsWithCallbacksMu.Lock()
	defer client.productsWithCallbacksMu.Unlock()
	client.productsWithCallbacks[product] = callback

	client.capabilitiesMu.Lock()
	defer client.capabilitiesMu.Unlock()
	for _, cap := range capabilities {
		client.capabilities[cap] = struct{}{}
	}
	return nil
}

// RegisterCallback allows registering a callback that will be invoked when the client
// receives configuration updates. It is up to that callback to then decide what to do
// depending on the product related to the configuration update.
func RegisterCallback(f Callback) error {
	if client == nil {
		return ErrClientNotStarted
	}
	client._callbacksMu.Lock()
	defer client._callbacksMu.Unlock()
	client.callbacks = append(client.callbacks, f)
	return nil
}

// UnregisterCallback removes a previously registered callback from the active callbacks list
// This remove operation preserves ordering
func UnregisterCallback(f Callback) error {
	if client == nil {
		return ErrClientNotStarted
	}
	client._callbacksMu.Lock()
	defer client._callbacksMu.Unlock()
	fValue := reflect.ValueOf(f)
	for i, callback := range client.callbacks {
		if reflect.ValueOf(callback) == fValue {
			client.callbacks = append(client.callbacks[:i], client.callbacks[i+1:]...)
			break
		}
	}
	return nil
}

// RegisterProduct adds a product to the list of products listened by the client
func RegisterProduct(p string) error {
	if client == nil {
		return ErrClientNotStarted
	}
	client.productsMu.Lock()
	defer client.productsMu.Unlock()
	client.productsWithCallbacksMu.RLock()
	defer client.productsWithCallbacksMu.RUnlock()
	if _, found := client.productsWithCallbacks[p]; found {
		return fmt.Errorf("product %s already registered via Subscribe", p)
	}
	client.products[p] = struct{}{}
	return nil
}

// UnregisterProduct removes a product from the list of products listened by the client
func UnregisterProduct(p string) error {
	if client == nil {
		return ErrClientNotStarted
	}
	client.productsMu.Lock()
	defer client.productsMu.Unlock()
	delete(client.products, p)
	return nil
}

// HasProduct returns whether a given product was registered
func HasProduct(p string) (bool, error) {
	if client == nil {
		return false, ErrClientNotStarted
	}
	client.productsMu.RLock()
	defer client.productsMu.RUnlock()
	client.productsWithCallbacksMu.RLock()
	defer client.productsWithCallbacksMu.RUnlock()
	_, found := client.products[p]
	_, foundWithCallback := client.productsWithCallbacks[p]
	return found || foundWithCallback, nil
}

// RegisterCapability adds a capability to the list of capabilities exposed by the client when requesting
// configuration updates
func RegisterCapability(cap Capability) error {
	if client == nil {
		return ErrClientNotStarted
	}
	client.capabilitiesMu.Lock()
	defer client.capabilitiesMu.Unlock()
	client.capabilities[cap] = struct{}{}
	return nil
}

// UnregisterCapability removes a capability from the list of capabilities exposed by the client when requesting
// configuration updates
func UnregisterCapability(cap Capability) error {
	if client == nil {
		return ErrClientNotStarted
	}
	client.capabilitiesMu.Lock()
	defer client.capabilitiesMu.Unlock()
	delete(client.capabilities, cap)
	return nil
}

// HasCapability returns whether a given capability was registered
func HasCapability(cap Capability) (bool, error) {
	if client == nil {
		return false, ErrClientNotStarted
	}
	client.capabilitiesMu.RLock()
	defer client.capabilitiesMu.RUnlock()
	_, found := client.capabilities[cap]
	return found, nil
}

func (c *Client) globalCallbacks() []Callback {
	c._callbacksMu.RLock()
	defer c._callbacksMu.RUnlock()
	callbacks := make([]Callback, len(c.callbacks))
	copy(callbacks, c.callbacks)
	return callbacks
}

func (c *Client) productCallbacks() map[string]ProductCallback {
	c.productsWithCallbacksMu.RLock()
	defer c.productsWithCallbacksMu.RUnlock()
	callbacks := make(map[string]ProductCallback, len(c.productsWithCallbacks))
	for k, v := range c.productsWithCallbacks {
		callbacks[k] = v
	}
	return callbacks
}

func (c *Client) allProducts() []string {
	c.productsMu.RLock()
	defer c.productsMu.RUnlock()
	c.productsWithCallbacksMu.RLock()
	defer c.productsWithCallbacksMu.RUnlock()
	products := make([]string, 0, len(c.products)+len(c.productsWithCallbacks))
	for p := range c.products {
		products = append(products, p)
	}
	for p := range c.productsWithCallbacks {
		products = append(products, p)
	}
	return products
}

func (c *Client) applyUpdate(pbUpdate *clientGetConfigsResponse) error {
	fileMap := make(map[string][]byte, len(pbUpdate.TargetFiles))
	allProducts := c.allProducts()
	productUpdates := make(map[string]ProductUpdate, len(allProducts))
	for _, p := range allProducts {
		productUpdates[p] = make(ProductUpdate)
	}
	for _, f := range pbUpdate.TargetFiles {
		fileMap[f.Path] = f.Raw
		for _, p := range allProducts {
			// Check the config file path to make sure it belongs to the right product
			if strings.Contains(f.Path, "/"+p+"/") {
				productUpdates[p][f.Path] = f.Raw
			}
		}
	}

	mapify := func(s *rc.RepositoryState) map[string]string {
		m := make(map[string]string)
		for i := range s.Configs {
			path := s.CachedFiles[i].Path
			product := s.Configs[i].Product
			m[path] = product
		}
		return m
	}

	// Check the repository state before and after the update to detect which configs are not being sent anymore.
	// This is needed because some products can stop sending configurations, and we want to make sure that the subscribers
	// are provided with this information in this case
	stateBefore, err := c.repository.CurrentState()
	if err != nil {
		return fmt.Errorf("repository current state error: %v", err)
	}
	products, err := c.repository.Update(rc.Update{
		TUFRoots:      pbUpdate.Roots,
		TUFTargets:    pbUpdate.Targets,
		TargetFiles:   fileMap,
		ClientConfigs: pbUpdate.ClientConfigs,
	})
	if err != nil {
		return fmt.Errorf("repository update error: %v", err)
	}
	stateAfter, err := c.repository.CurrentState()
	if err != nil {
		return fmt.Errorf("repository current state error after update: %v", err)
	}

	// Create a config files diff between before/after the update to see which config files are missing
	mBefore := mapify(&stateBefore)
	for k := range mapify(&stateAfter) {
		delete(mBefore, k)
	}

	// Set the payload data to nil for missing config files. The callbacks then can handle the nil config case to detect
	// that this config will not be updated anymore.
	updatedProducts := make(map[string]struct{})
	for path, product := range mBefore {
		if productUpdates[product] == nil {
			productUpdates[product] = make(ProductUpdate)
		}
		productUpdates[product][path] = nil
		updatedProducts[product] = struct{}{}
	}
	// Aggregate updated products and missing products so that callbacks get called for both
	for _, p := range products {
		updatedProducts[p] = struct{}{}
	}

	if len(updatedProducts) == 0 {
		return nil
	}
	// Performs the callbacks registered and update the application status in the repository (RCTE2)
	// In case of several callbacks handling the same config, statuses take precedence in this order:
	// 1 - ApplyStateError
	// 2 - ApplyStateUnacknowledged
	// 3 - ApplyStateAcknowledged
	// This makes sure that any product that would need to re-receive the config in a subsequent update will be allowed to
	statuses := make(map[string]rc.ApplyStatus)
	for _, fn := range c.globalCallbacks() {
		for path, status := range fn(productUpdates) {
			if s, ok := statuses[path]; !ok || status.State == rc.ApplyStateError ||
				s.State == rc.ApplyStateAcknowledged && status.State == rc.ApplyStateUnacknowledged {
				statuses[path] = status
			}
		}
	}
	// Call the product-specific callbacks registered via Subscribe
	productCallbacks := c.productCallbacks()
	for product, update := range productUpdates {
		if fn, ok := productCallbacks[product]; ok {
			for path, status := range fn(update) {
				statuses[path] = status
			}
		}
	}
	for p, s := range statuses {
		c.repository.UpdateApplyStatus(p, s)
	}

	return nil
}

func (c *Client) newUpdateRequest() (bytes.Buffer, error) {
	state, err := c.repository.CurrentState()
	if err != nil {
		return bytes.Buffer{}, err
	}
	// Temporary check while using untrusted repo, for which no initial root file is provided
	if state.RootsVersion < 1 {
		state.RootsVersion = 1
	}

	pbCachedFiles := make([]*targetFileMeta, 0, len(state.CachedFiles))
	for _, f := range state.CachedFiles {
		pbHashes := make([]*targetFileHash, 0, len(f.Hashes))
		for alg, hash := range f.Hashes {
			pbHashes = append(pbHashes, &targetFileHash{
				Algorithm: alg,
				Hash:      hex.EncodeToString(hash),
			})
		}
		pbCachedFiles = append(pbCachedFiles, &targetFileMeta{
			Path:   f.Path,
			Length: int64(f.Length),
			Hashes: pbHashes,
		})
	}

	hasError := c.lastError != nil
	errMsg := ""
	if hasError {
		errMsg = c.lastError.Error()
	}

	var pbConfigState []*configState
	if !hasError {
		pbConfigState = make([]*configState, 0, len(state.Configs))
		for _, f := range state.Configs {
			pbConfigState = append(pbConfigState, &configState{
				ID:         f.ID,
				Version:    f.Version,
				Product:    f.Product,
				ApplyState: f.ApplyStatus.State,
				ApplyError: f.ApplyStatus.Error,
			})
		}
	}

	capa := big.NewInt(0)
	for i := range c.capabilities {
		capa.SetBit(capa, int(i), 1)
	}
	req := clientGetConfigsRequest{
		Client: &clientData{
			State: &clientState{
				RootVersion:    uint64(state.RootsVersion),
				TargetsVersion: uint64(state.TargetsVersion),
				ConfigStates:   pbConfigState,
				HasError:       hasError,
				Error:          errMsg,
			},
			ID:       c.clientID,
			Products: c.allProducts(),
			IsTracer: true,
			ClientTracer: &clientTracer{
				RuntimeID:     c.RuntimeID,
				Language:      "go",
				TracerVersion: c.TracerVersion,
				Service:       c.ServiceName,
				Env:           c.Env,
				AppVersion:    c.AppVersion,
			},
			Capabilities: capa.Bytes(),
		},
		CachedTargetFiles: pbCachedFiles,
	}

	var b bytes.Buffer

	err = json.NewEncoder(&b).Encode(&req)
	if err != nil {
		return bytes.Buffer{}, err
	}

	return b, nil
}

var (
	idSize     = 21
	idAlphabet = []rune("_-0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ")
)

func generateID() string {
	bytes := make([]byte, idSize)
	_, err := rand.Read(bytes)
	if err != nil {
		panic(err)
	}
	id := make([]rune, idSize)
	for i := 0; i < idSize; i++ {
		id[i] = idAlphabet[bytes[i]&63]
	}
	return string(id[:idSize])
}
