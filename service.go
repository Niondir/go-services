// Package services defines interfaces and methods to run background services in golang applications.
//
// A Service is a somewhat independently running piece of code that runs in it's own go-routine
// it's initialized at some point and stopped later. Think of it as a deamon within the application.
//
// All services are registered during init() or in main() and initialized all together by calling Container.StartAll()
// Services that implement the Initer interface, will run initial Init() code
// All services have to implement the Runner interface. Run() is blocking and only returns when the service stops working.
//
// All services inside one container are started and stopped together. If one service fails, all are stopped.
package services

import (
	"context"
	"fmt"
	"github.com/sirupsen/logrus"
	"strings"
	"sync"
	"time"
)

type RunFunc func(ctx context.Context) error
type InitFunc func(ctx context.Context) error

type startRunner struct {
	name string
	init InitFunc
	run  RunFunc
}

func (sr *startRunner) Init(ctx context.Context) error {
	return sr.init(ctx)
}
func (sr *startRunner) Run(ctx context.Context) error {
	return sr.run(ctx)
}

func (sr *startRunner) String() string {
	return sr.name
}

type runContext struct {
	service *serviceInfo
	running bool
	done    chan error
	err     error
}

type serviceInfo struct {
	name    string
	service Runner
}

func (rc *runContext) Wait() {
	if !rc.running {
		return
	}
	rc.err = <-rc.done
	rc.running = false
}

// Container with all services
// The Container handles the following lifecycle:
// - Register all services
// - Start all services
// - Stop all services
// If a single service fails during init or run, all services inside the container are stopped.
type Container struct {
	// Context in which all services are running
	runCtx context.Context
	// Cancel method of the runCtx
	runCtxCancel context.CancelFunc
	services     []*serviceInfo
	runContexts  map[string]*runContext
}

func NewContainer() *Container {
	return &Container{
		services:    make([]*serviceInfo, 0),
		runContexts: map[string]*runContext{},
	}
}

var defaultContainer *Container

func Default() *Container {
	if defaultContainer == nil {
		defaultContainer = NewContainer()
	}
	return defaultContainer
}

// Register adds a service to the list of services to be initialized
func (c *Container) Register(service Runner) {
	name := fmt.Sprintf("%T", service)
	if s, ok := service.(fmt.Stringer); ok {
		name = s.String()
	}

	for _, s := range c.services {
		if s.name == name {
			panic(fmt.Sprintf("Service '%s' already registered", name))
		}
	}

	c.services = append(c.services, &serviceInfo{
		name:    name,
		service: service,
	})
}

// FuncService is a wrapper that turns a func() into a service.Runner
type FuncService func(ctx context.Context) error

func (f FuncService) Run(ctx context.Context) error {
	return f(ctx)
}

func newRunContext(s *serviceInfo) *runContext {
	return &runContext{
		service: s,
		done:    make(chan error, 1),
	}
}

func (c *Container) initOne(ctx context.Context, s *serviceInfo) error {
	logger := logrus.WithField("service", s.name)
	runner := newRunContext(s)
	if _, ok := c.runContexts[s.name]; ok {
		return fmt.Errorf("service '%s' already started", s.name)
	}

	c.runContexts[s.name] = runner

	// Execute initialization code if any
	if starter, ok := s.service.(Initer); ok {
		logger.Info("Execute service.Init()")
		err := starter.Init(ctx)
		if err != nil {
			go func() {
				// Let the runner stop immediately
				// The error is nil, since it is the "Run()" error
				runner.done <- nil
			}()
			return fmt.Errorf("failed to init service %s: %w", s.name, err)
		}
	}

	return nil
}

func (c *Container) runOne(ctx context.Context, s *serviceInfo) error {
	logger := logrus.WithField("service", s.name)

	runner, ok := c.runContexts[s.name]
	if !ok {
		return fmt.Errorf("service '%s' not initialized", s.name)
	}
	if runner.running {
		return fmt.Errorf("service '%s' already running", s.name)
	}

	// Execute the actual run method in background
	runner.running = true
	go func() {
		logger.Info("Execute service.Run()")
		runErr := s.service.Run(ctx)
		runner.done <- runErr
		if runErr != nil {
			// TODO: Make this optional / configurable?
			logger.WithError(runErr).Error("Service stopped with error. Stop all services.")
			c.StopAll()
		} else {
			logger.Error("Service stopped")
		}
	}()

	return nil
}

func (c *Container) StartAll(ctx context.Context) error {
	if c.runCtx != nil {
		panic("Container.StartAll can only be called once")
	}
	c.runCtx, c.runCtxCancel = context.WithCancel(ctx)

	// Iterate over all services to initialize them
	for i := range c.services {
		s := c.services[i]
		logger := logrus.WithField("service", s.name)
		// TODO: Should we allow services to optionally initialize in parallel?
		logger.Infof("Initialize service %d/%d", i+1, len(c.services))

		err := c.initOne(c.runCtx, s)
		if err != nil {
			logger.Errorf("Failed to initialize service.")
			c.runCtxCancel()
			return err
		}
	}

	// Iterate over all services to run them
	for i := range c.services {
		s := c.services[i]
		logger := logrus.WithField("service", s.name)
		logger.Infof("Run service %d/%d", i+1, len(c.services))

		err := c.runOne(c.runCtx, s)
		if err != nil {
			logger.WithError(err).Errorf("Failed to start service.")
			c.runCtxCancel()
			return err
		}
	}

	logrus.Info("All services running")
	return nil
}

// StopAll gracefully stops all services.
// If you need a timeout, passe a context with Timeout or Deadline
func (c *Container) StopAll() {
	if c.runCtxCancel == nil {
		panic("call Container.StartAll() before StopAll()")
	}
	c.runCtxCancel()
}

func (c *Container) runningServices() []*runContext {
	rcs := make([]*runContext, 0)
	for i := range c.runContexts {
		rc := c.runContexts[i]
		if rc.running {
			rcs = append(rcs, rc)
		}
	}
	return rcs
}

// WaitAllStopped blocks until all services are stopped or context is exceeded
func (c *Container) WaitAllStopped(ctx context.Context) {
	ctx, cancel := context.WithCancel(ctx)

	wg := sync.WaitGroup{}
	logrus.WithField("count", len(c.runContexts)).Infof("Wait till all services are stopped")
	wg.Add(len(c.runContexts))
	for k := range c.runContexts {
		rc := c.runContexts[k]
		logger := logrus.WithField("service", rc.service.name)
		go func() {
			logger.Info("Stopping service")
			rc.Wait()
			if rc.err != nil {
				logger.WithError(rc.err).Warn("Service stopped with error")
			}

			wg.Done()
		}()
	}

	// Really just logging ... remove?
	go func() {
		for {
			select {
			case <-time.After(1 * time.Second):
				c.runningServicesLogger().Info("Waiting for services to stop")
			case <-ctx.Done():
				break
			}
		}
	}()

	// Wait till all services are stopped
	go func() {
		wg.Wait()
		cancel()
	}()

	<-ctx.Done()

	if ctx.Err() == context.DeadlineExceeded {
		c.runningServicesLogger().Warn("Services did not stopped gracefully!")
	}
}

// ServiceErrors returns all errors occured in services
func (c *Container) ServiceErrors() map[string]error {
	errs := map[string]error{}
	for _, rc := range c.runContexts {
		if rc.err != nil {
			errs[rc.service.name] = rc.err
		}
	}
	return errs
}

func (c *Container) runningServicesLogger() *logrus.Entry {
	rcs := c.runningServices()
	names := make([]string, len(rcs))
	for i := range rcs {
		names[i] = rcs[i].service.name
	}
	namesJoined := strings.Join(names, ",")
	return logrus.WithField("count", len(rcs)).
		WithField("services", namesJoined)
}
