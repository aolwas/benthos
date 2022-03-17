package manager

import (
	"context"
	"fmt"
	"net/http"
	"path"
	"sync"
	"time"

	"github.com/benthosdev/benthos/v4/internal/bloblang"
	"github.com/benthosdev/benthos/v4/internal/bloblang/query"
	"github.com/benthosdev/benthos/v4/internal/bundle"
	"github.com/benthosdev/benthos/v4/internal/component"
	"github.com/benthosdev/benthos/v4/internal/component/buffer"
	"github.com/benthosdev/benthos/v4/internal/component/cache"
	iinput "github.com/benthosdev/benthos/v4/internal/component/input"
	"github.com/benthosdev/benthos/v4/internal/component/metrics"
	ioutput "github.com/benthosdev/benthos/v4/internal/component/output"
	iprocessor "github.com/benthosdev/benthos/v4/internal/component/processor"
	"github.com/benthosdev/benthos/v4/internal/component/ratelimit"
	"github.com/benthosdev/benthos/v4/internal/docs"
	"github.com/benthosdev/benthos/v4/internal/interop"
	"github.com/benthosdev/benthos/v4/internal/log"
	"github.com/benthosdev/benthos/v4/internal/message"
	"github.com/benthosdev/benthos/v4/internal/old/input"
	"github.com/benthosdev/benthos/v4/internal/old/output"
	"github.com/benthosdev/benthos/v4/internal/old/processor"
)

// ErrResourceNotFound represents an error where a named resource could not be
// accessed because it was not found by the manager.
type ErrResourceNotFound string

// Error implements the standard error interface.
func (e ErrResourceNotFound) Error() string {
	return fmt.Sprintf("unable to locate resource: %v", string(e))
}

//------------------------------------------------------------------------------

// APIReg is an interface representing an API builder.
type APIReg interface {
	RegisterEndpoint(path, desc string, h http.HandlerFunc)
}

//------------------------------------------------------------------------------

// Type is an implementation of types.Manager, which is expected by Benthos
// components that need to register service wide behaviours such as HTTP
// endpoints and event listeners, and obtain service wide shared resources such
// as caches and other resources.
type Type struct {
	// An optional identifier given to a manager that is used by a unique stream
	// and if specified should be used as a path prefix for API endpoints, and
	// added as a label to logs and metrics.
	stream string

	// Keeps track of the full configuration path of the component that holds
	// the manager. This value is used only in observability and therefore it
	// is acceptable that this does not fully represent reality.
	componentPath []string

	// Keeps track of the label of the component holding this manager.
	label string

	apiReg APIReg

	inputs       map[string]*inputWrapper
	caches       map[string]cache.V1
	processors   map[string]iprocessor.V1
	outputs      map[string]*outputWrapper
	rateLimits   map[string]ratelimit.V1
	resourceLock *sync.RWMutex

	// Collections of component constructors
	env      *bundle.Environment
	bloblEnv *bloblang.Environment

	logger log.Modular
	stats  *metrics.Namespaced

	pipes    map[string]<-chan message.Transaction
	pipeLock *sync.RWMutex
}

// OptFunc is an opt setting for a manager type.
type OptFunc func(*Type)

// OptSetEnvironment determines the environment from which the manager
// initializes components and resources. This option is for internal use only.
func OptSetEnvironment(e *bundle.Environment) OptFunc {
	return func(t *Type) {
		t.env = e
	}
}

// OptSetBloblangEnvironment determines the environment from which the manager
// parses bloblang functions and methods. This option is for internal use only.
func OptSetBloblangEnvironment(env *bloblang.Environment) OptFunc {
	return func(t *Type) {
		t.bloblEnv = env
	}
}

// NewV2 returns an instance of manager.Type, which can be shared amongst
// components and logical threads of a Benthos service.
func NewV2(conf ResourceConfig, apiReg APIReg, log log.Modular, stats *metrics.Namespaced, opts ...OptFunc) (*Type, error) {
	t := &Type{
		apiReg: apiReg,

		inputs:       map[string]*inputWrapper{},
		caches:       map[string]cache.V1{},
		processors:   map[string]iprocessor.V1{},
		outputs:      map[string]*outputWrapper{},
		rateLimits:   map[string]ratelimit.V1{},
		resourceLock: &sync.RWMutex{},

		// Environment defaults to global (everything that was imported).
		env:      bundle.GlobalEnvironment,
		bloblEnv: bloblang.GlobalEnvironment(),

		logger: log,
		stats:  stats,

		pipes:    map[string]<-chan message.Transaction{},
		pipeLock: &sync.RWMutex{},
	}

	for _, opt := range opts {
		opt(t)
	}

	seen := map[string]struct{}{}

	checkLabel := func(typeStr, label string) error {
		if label == "" {
			return fmt.Errorf("%v resource has an empty label", typeStr)
		}
		if _, exists := seen[label]; exists {
			return fmt.Errorf("%v resource label '%v' collides with a previously defined resource", typeStr, label)
		}
		seen[label] = struct{}{}
		return nil
	}

	// Sometimes resources of a type might refer to other resources of the same
	// type. When they are constructed they will check with the manager to
	// ensure the resource they point to is valid, but not keep the reference.
	// Since we cannot guarantee an order of initialisation we create
	// placeholders during construction.
	for _, c := range conf.ResourceInputs {
		if err := checkLabel("input", c.Label); err != nil {
			return nil, err
		}
		t.inputs[c.Label] = nil
	}
	for _, c := range conf.ResourceCaches {
		if err := checkLabel("cache", c.Label); err != nil {
			return nil, err
		}
		t.caches[c.Label] = nil
	}
	for _, c := range conf.ResourceProcessors {
		if err := checkLabel("processor", c.Label); err != nil {
			return nil, err
		}
		t.processors[c.Label] = nil
	}
	for _, c := range conf.ResourceOutputs {
		if err := checkLabel("output", c.Label); err != nil {
			return nil, err
		}
		t.outputs[c.Label] = nil
	}
	for _, c := range conf.ResourceRateLimits {
		if err := checkLabel("rate limit", c.Label); err != nil {
			return nil, err
		}
		t.rateLimits[c.Label] = nil
	}

	// Labels validated, begin construction
	for _, conf := range conf.ResourceRateLimits {
		if err := t.StoreRateLimit(context.Background(), conf.Label, conf); err != nil {
			return nil, err
		}
	}

	for _, conf := range conf.ResourceCaches {
		if err := t.StoreCache(context.Background(), conf.Label, conf); err != nil {
			return nil, err
		}
	}

	// TODO: Prevent recursive processors.
	for _, conf := range conf.ResourceProcessors {
		if err := t.StoreProcessor(context.Background(), conf.Label, conf); err != nil {
			return nil, err
		}
	}

	for _, conf := range conf.ResourceInputs {
		if err := t.StoreInput(context.Background(), conf.Label, conf); err != nil {
			return nil, err
		}
	}

	for _, conf := range conf.ResourceOutputs {
		if err := t.StoreOutput(context.Background(), conf.Label, conf); err != nil {
			return nil, err
		}
	}

	return t, nil
}

//------------------------------------------------------------------------------

// ForStream returns a variant of this manager to be used by a particular stream
// identifer, where APIs registered will be namespaced by that id.
func (t *Type) ForStream(id string) interop.Manager {
	return t.forStream(id)
}

func (t *Type) forStream(id string) *Type {
	newT := *t
	newT.stream = id
	newT.logger = t.logger.WithFields(map[string]string{
		"stream": id,
	})
	newT.stats = t.stats.WithLabels("stream", id)
	return &newT
}

func (t *Type) forLabel(name string) *Type {
	newT := *t
	newT.label = name
	newT.logger = t.logger.WithFields(map[string]string{
		"label": name,
	})
	newT.stats = t.stats.WithLabels("label", name)
	return &newT
}

// IntoPath returns a variant of this manager to be used by a particular
// component path, which is a child of the current component, where
// observability components will be automatically tagged with the new path.
func (t *Type) IntoPath(segments ...string) interop.Manager {
	return t.intoPath(segments...)
}

func (t *Type) intoPath(segments ...string) *Type {
	newT := *t
	newComponentPath := make([]string, 0, len(t.componentPath)+len(segments))
	newComponentPath = append(newComponentPath, t.componentPath...)
	newComponentPath = append(newComponentPath, segments...)
	newT.componentPath = newComponentPath

	pathStr := "root." + query.SliceToDotPath(newComponentPath...)
	newT.logger = t.logger.WithFields(map[string]string{
		"path": pathStr,
	})
	newT.stats = t.stats.WithLabels("path", pathStr)
	return &newT
}

// Path returns the current component path held by a manager.
func (t *Type) Path() []string {
	return t.componentPath
}

// Label returns the current component label held by a manager.
func (t *Type) Label() string {
	return t.label
}

// WithAddedMetrics returns a modified version of the manager where metrics are
// registered to both the current metrics target as well as the provided one.
func (t *Type) WithAddedMetrics(m metrics.Type) interop.Manager {
	newT := *t
	newT.stats = newT.stats.WithStats(metrics.Combine(newT.stats.Child(), m))
	return &newT
}

//------------------------------------------------------------------------------

// RegisterEndpoint registers a server wide HTTP endpoint.
func (t *Type) RegisterEndpoint(apiPath, desc string, h http.HandlerFunc) {
	if len(t.stream) > 0 {
		apiPath = path.Join("/", t.stream, apiPath)
	}
	if t.apiReg != nil {
		t.apiReg.RegisterEndpoint(apiPath, desc, h)
	}
}

// SetPipe registers a new transaction chan to a named pipe.
func (t *Type) SetPipe(name string, tran <-chan message.Transaction) {
	t.pipeLock.Lock()
	t.pipes[name] = tran
	t.pipeLock.Unlock()
}

// GetPipe attempts to obtain and return a named output Pipe
func (t *Type) GetPipe(name string) (<-chan message.Transaction, error) {
	t.pipeLock.RLock()
	pipe, exists := t.pipes[name]
	t.pipeLock.RUnlock()
	if exists {
		return pipe, nil
	}
	return nil, component.ErrPipeNotFound
}

// UnsetPipe removes a named pipe transaction chan.
func (t *Type) UnsetPipe(name string, tran <-chan message.Transaction) {
	t.pipeLock.Lock()
	if otran, exists := t.pipes[name]; exists && otran == tran {
		delete(t.pipes, name)
	}
	t.pipeLock.Unlock()
}

//------------------------------------------------------------------------------

// WithMetricsMapping returns a manager with the stored metrics exporter wrapped
// with a mapping.
func (t *Type) WithMetricsMapping(m *metrics.Mapping) *Type {
	newT := *t
	newT.stats = t.stats.WithMapping(m)
	return &newT
}

// Metrics returns an aggregator preset with the current component context.
func (t *Type) Metrics() metrics.Type {
	return t.stats
}

// Logger returns a logger preset with the current component context.
func (t *Type) Logger() log.Modular {
	return t.logger
}

// Environment returns a bundle environment used by the manager. This is for
// internal use only.
func (t *Type) Environment() *bundle.Environment {
	return t.env
}

// BloblEnvironment returns a Bloblang environment used by the manager. This is
// for internal use only.
func (t *Type) BloblEnvironment() *bloblang.Environment {
	return t.bloblEnv
}

//------------------------------------------------------------------------------

// GetDocs returns a documentation spec for an implementation of a component.
func (t *Type) GetDocs(name string, ctype docs.Type) (docs.ComponentSpec, bool) {
	return t.env.GetDocs(name, ctype)
}

//------------------------------------------------------------------------------

type oldClosable interface {
	CloseAsync()
	WaitForClose(timeout time.Duration) error
}

func closeWithContext(ctx context.Context, c oldClosable) error {
	c.CloseAsync()
	waitFor := time.Second
	deadline, hasDeadline := ctx.Deadline()
	if hasDeadline {
		waitFor = time.Until(deadline)
	}
	err := c.WaitForClose(waitFor)
	for err != nil && !hasDeadline {
		err = c.WaitForClose(time.Second)
	}
	return err
}

//------------------------------------------------------------------------------

// NewBuffer attempts to create a new buffer component from a config.
func (t *Type) NewBuffer(conf buffer.Config) (buffer.Streamed, error) {
	return t.env.BufferInit(conf, t)
}

//------------------------------------------------------------------------------

// ProbeCache returns true if a cache resource exists under the provided name.
func (t *Type) ProbeCache(name string) bool {
	_, exists := t.caches[name]
	return exists
}

// AccessCache attempts to access a cache resource by a unique identifier and
// executes a closure function with the cache as an argument. Returns an error
// if the cache does not exist (or is otherwise inaccessible).
//
// During the execution of the provided closure it is guaranteed that the
// resource will not be closed or removed. However, it is possible for the
// resource to be accessed by any number of components in parallel.
func (t *Type) AccessCache(ctx context.Context, name string, fn func(cache.V1)) error {
	// TODO: Eventually use ctx to cancel blocking on the mutex lock. Needs
	// profiling for heavy use within a busy loop.
	t.resourceLock.RLock()
	defer t.resourceLock.RUnlock()
	c, ok := t.caches[name]
	if !ok || c == nil {
		return ErrResourceNotFound(name)
	}
	fn(c)
	return nil
}

// NewCache attempts to create a new cache component from a config.
func (t *Type) NewCache(conf cache.Config) (cache.V1, error) {
	return t.env.CacheInit(conf, t.forLabel(conf.Label))
}

// StoreCache attempts to store a new cache resource. If an existing resource
// has the same name it is closed and removed _before_ the new one is
// initialized in order to avoid duplicate connections.
func (t *Type) StoreCache(ctx context.Context, name string, conf cache.Config) error {
	t.resourceLock.Lock()
	defer t.resourceLock.Unlock()

	c, ok := t.caches[name]
	if ok && c != nil {
		// If a previous resource exists with the same name then we do NOT allow
		// it to be replaced unless it can be successfully closed. This ensures
		// that we do not leak connections.
		if err := c.Close(ctx); err != nil {
			return err
		}
	}

	newCache, err := t.intoPath("cache_resources").NewCache(conf)
	if err != nil {
		return fmt.Errorf(
			"failed to create cache resource '%v' of type '%v': %w",
			name, conf.Type, err,
		)
	}

	t.caches[name] = newCache
	return nil
}

//------------------------------------------------------------------------------

// ProbeInput returns true if an input resource exists under the provided name.
func (t *Type) ProbeInput(name string) bool {
	_, exists := t.inputs[name]
	return exists
}

// AccessInput attempts to access an input resource by a unique identifier and
// executes a closure function with the input as an argument. Returns an error
// if the input does not exist (or is otherwise inaccessible).
//
// During the execution of the provided closure it is guaranteed that the
// resource will not be closed or removed. However, it is possible for the
// resource to be accessed by any number of components in parallel.
func (t *Type) AccessInput(ctx context.Context, name string, fn func(iinput.Streamed)) error {
	// TODO: Eventually use ctx to cancel blocking on the mutex lock. Needs
	// profiling for heavy use within a busy loop.
	t.resourceLock.RLock()
	defer t.resourceLock.RUnlock()
	i, ok := t.inputs[name]
	if !ok || i == nil {
		return ErrResourceNotFound(name)
	}
	fn(i)
	return nil
}

// NewInput attempts to create a new input component from a config.
func (t *Type) NewInput(conf input.Config, pipelines ...iprocessor.PipelineConstructorFunc) (iinput.Streamed, error) {
	return t.env.InputInit(conf, t.forLabel(conf.Label), pipelines...)
}

// StoreInput attempts to store a new input resource. If an existing resource
// has the same name it is closed and removed _before_ the new one is
// initialized in order to avoid duplicate connections.
func (t *Type) StoreInput(ctx context.Context, name string, conf input.Config) error {
	t.resourceLock.Lock()
	defer t.resourceLock.Unlock()

	i, exists := t.inputs[name]
	if exists && i != nil {
		// If a previous resource exists with the same name then we do NOT allow
		// it to be replaced unless it can be successfully closed. This ensures
		// that we do not leak connections.
		if err := i.closeExistingInput(ctx); err != nil {
			return err
		}
	}

	if conf.Label != "" && conf.Label != name {
		return fmt.Errorf("label '%v' must be empty or match the resource name '%v'", conf.Label, name)
	}

	newInput, err := t.intoPath("input_resources").NewInput(conf)
	if err != nil {
		return fmt.Errorf(
			"failed to create input resource '%v' of type '%v': %w",
			name, conf.Type, err,
		)
	}

	if exists && i != nil {
		i.swapInput(newInput)
	} else {
		t.inputs[name] = wrapInput(newInput)
	}
	return nil
}

//------------------------------------------------------------------------------

// ProbeProcessor returns true if a processor resource exists under the provided
// name.
func (t *Type) ProbeProcessor(name string) bool {
	_, exists := t.processors[name]
	return exists
}

// AccessProcessor attempts to access a processor resource by a unique
// identifier and executes a closure function with the processor as an argument.
// Returns an error if the processor does not exist (or is otherwise
// inaccessible).
//
// During the execution of the provided closure it is guaranteed that the
// resource will not be closed or removed. However, it is possible for the
// resource to be accessed by any number of components in parallel.
func (t *Type) AccessProcessor(ctx context.Context, name string, fn func(iprocessor.V1)) error {
	// TODO: Eventually use ctx to cancel blocking on the mutex lock. Needs
	// profiling for heavy use within a busy loop.
	t.resourceLock.RLock()
	defer t.resourceLock.RUnlock()
	p, ok := t.processors[name]
	if !ok || p == nil {
		return ErrResourceNotFound(name)
	}
	fn(p)
	return nil
}

// NewProcessor attempts to create a new processor component from a config.
func (t *Type) NewProcessor(conf processor.Config) (iprocessor.V1, error) {
	return t.env.ProcessorInit(conf, t.forLabel(conf.Label))
}

// StoreProcessor attempts to store a new processor resource. If an existing
// resource has the same name it is closed and removed _before_ the new one is
// initialized in order to avoid duplicate connections.
func (t *Type) StoreProcessor(ctx context.Context, name string, conf processor.Config) error {
	t.resourceLock.Lock()
	defer t.resourceLock.Unlock()

	p, ok := t.processors[name]
	if ok && p != nil {
		// If a previous resource exists with the same name then we do NOT allow
		// it to be replaced unless it can be successfully closed. This ensures
		// that we do not leak connections.
		if err := closeWithContext(ctx, p); err != nil {
			return err
		}
	}

	if conf.Label != "" && conf.Label != name {
		return fmt.Errorf("label '%v' must be empty or match the resource name '%v'", conf.Label, name)
	}

	newProcessor, err := t.intoPath("processor_resources").NewProcessor(conf)
	if err != nil {
		return fmt.Errorf(
			"failed to create processor resource '%v' of type '%v': %w",
			name, conf.Type, err,
		)
	}

	t.processors[name] = newProcessor
	return nil
}

//------------------------------------------------------------------------------

// ProbeOutput returns true if an output resource exists under the provided
// name.
func (t *Type) ProbeOutput(name string) bool {
	_, exists := t.outputs[name]
	return exists
}

// AccessOutput attempts to access an output resource by a unique identifier and
// executes a closure function with the output as an argument. Returns an error
// if the output does not exist (or is otherwise inaccessible).
//
// During the execution of the provided closure it is guaranteed that the
// resource will not be closed or removed. However, it is possible for the
// resource to be accessed by any number of components in parallel.
func (t *Type) AccessOutput(ctx context.Context, name string, fn func(ioutput.Sync)) error {
	// TODO: Eventually use ctx to cancel blocking on the mutex lock. Needs
	// profiling for heavy use within a busy loop.
	t.resourceLock.RLock()
	defer t.resourceLock.RUnlock()
	o, ok := t.outputs[name]
	if !ok || o == nil {
		return ErrResourceNotFound(name)
	}
	fn(o)
	return nil
}

// NewOutput attempts to create a new output component from a config.
func (t *Type) NewOutput(conf output.Config, pipelines ...iprocessor.PipelineConstructorFunc) (ioutput.Streamed, error) {
	return t.env.OutputInit(conf, t.forLabel(conf.Label), pipelines...)
}

// StoreOutput attempts to store a new output resource. If an existing resource
// has the same name it is closed and removed _before_ the new one is
// initialized in order to avoid duplicate connections.
func (t *Type) StoreOutput(ctx context.Context, name string, conf output.Config) error {
	t.resourceLock.Lock()
	defer t.resourceLock.Unlock()

	o, ok := t.outputs[name]
	if ok && o != nil {
		// If a previous resource exists with the same name then we do NOT allow
		// it to be replaced unless it can be successfully closed. This ensures
		// that we do not leak connections.
		if err := closeWithContext(ctx, o); err != nil {
			return err
		}
	}

	if conf.Label != "" && conf.Label != name {
		return fmt.Errorf("label '%v' must be empty or match the resource name '%v'", conf.Label, name)
	}

	tmpOutput, err := t.intoPath("output_resources").NewOutput(conf)
	if err == nil {
		if t.outputs[name], err = wrapOutput(tmpOutput); err != nil {
			tmpOutput.CloseAsync()
		}
	}
	if err != nil {
		return fmt.Errorf(
			"failed to create output resource '%v' of type '%v': %w",
			name, conf.Type, err,
		)
	}
	return nil
}

//------------------------------------------------------------------------------

// ProbeRateLimit returns true if a rate limit resource exists under the
// provided name.
func (t *Type) ProbeRateLimit(name string) bool {
	_, exists := t.rateLimits[name]
	return exists
}

// AccessRateLimit attempts to access a rate limit resource by a unique
// identifier and executes a closure function with the rate limit as an
// argument. Returns an error if the rate limit does not exist (or is otherwise
// inaccessible).
//
// During the execution of the provided closure it is guaranteed that the
// resource will not be closed or removed. However, it is possible for the
// resource to be accessed by any number of components in parallel.
func (t *Type) AccessRateLimit(ctx context.Context, name string, fn func(ratelimit.V1)) error {
	// TODO: Eventually use ctx to cancel blocking on the mutex lock. Needs
	// profiling for heavy use within a busy loop.
	t.resourceLock.RLock()
	defer t.resourceLock.RUnlock()
	r, ok := t.rateLimits[name]
	if !ok || r == nil {
		return ErrResourceNotFound(name)
	}
	fn(r)
	return nil
}

// NewRateLimit attempts to create a new rate limit component from a config.
func (t *Type) NewRateLimit(conf ratelimit.Config) (ratelimit.V1, error) {
	return t.env.RateLimitInit(conf, t.forLabel(conf.Label))
}

// StoreRateLimit attempts to store a new rate limit resource. If an existing
// resource has the same name it is closed and removed _before_ the new one is
// initialized in order to avoid duplicate connections.
func (t *Type) StoreRateLimit(ctx context.Context, name string, conf ratelimit.Config) error {
	t.resourceLock.Lock()
	defer t.resourceLock.Unlock()

	r, ok := t.rateLimits[name]
	if ok && r != nil {
		// If a previous resource exists with the same name then we do NOT allow
		// it to be replaced unless it can be successfully closed. This ensures
		// that we do not leak connections.
		if err := r.Close(ctx); err != nil {
			return err
		}
	}

	newRateLimit, err := t.intoPath("rate_limit_resources").NewRateLimit(conf)
	if err != nil {
		return fmt.Errorf(
			"failed to create rate limit resource '%v' of type '%v': %w",
			name, conf.Type, err,
		)
	}

	t.rateLimits[name] = newRateLimit
	return nil
}

//------------------------------------------------------------------------------

// CloseAsync triggers the shut down of all resource types that implement the
// lifetime interface types.Closable.
func (t *Type) CloseAsync() {
	t.resourceLock.Lock()
	defer t.resourceLock.Unlock()

	for _, c := range t.inputs {
		c.CloseAsync()
	}
	for _, p := range t.processors {
		p.CloseAsync()
	}
	for _, c := range t.outputs {
		c.CloseAsync()
	}
}

// WaitForClose blocks until either all closable resource types are shut down or
// a timeout occurs.
func (t *Type) WaitForClose(timeout time.Duration) error {
	t.resourceLock.Lock()
	defer t.resourceLock.Unlock()

	tOutCtx, done := context.WithTimeout(context.Background(), timeout)
	defer done()

	timesOut := time.Now().Add(timeout)
	for k, c := range t.inputs {
		if err := c.WaitForClose(time.Until(timesOut)); err != nil {
			return fmt.Errorf("resource '%s' failed to cleanly shutdown: %v", k, err)
		}
		delete(t.inputs, k)
	}
	for k, c := range t.caches {
		if err := c.Close(tOutCtx); err != nil {
			return fmt.Errorf("resource '%s' failed to cleanly shutdown: %v", k, err)
		}
		delete(t.caches, k)
	}
	for k, p := range t.processors {
		if err := p.WaitForClose(time.Until(timesOut)); err != nil {
			return fmt.Errorf("resource '%s' failed to cleanly shutdown: %v", k, err)
		}
		delete(t.processors, k)
	}
	for k, c := range t.rateLimits {
		if err := c.Close(tOutCtx); err != nil {
			return fmt.Errorf("resource '%s' failed to cleanly shutdown: %v", k, err)
		}
		delete(t.rateLimits, k)
	}
	for k, c := range t.outputs {
		if err := c.WaitForClose(time.Until(timesOut)); err != nil {
			return fmt.Errorf("resource '%s' failed to cleanly shutdown: %v", k, err)
		}
		delete(t.outputs, k)
	}
	return nil
}
