/***** BEGIN LICENSE BLOCK *****
# This Source Code Form is subject to the terms of the Mozilla Public
# License, v. 2.0. If a copy of the MPL was not distributed with this file,
# You can obtain one at http://mozilla.org/MPL/2.0/.
#
# The Initial Developer of the Original Code is the Mozilla Foundation.
# Portions created by the Initial Developer are Copyright (C) 2012-2014
# the Initial Developer. All Rights Reserved.
#
# Contributor(s):
#   Rob Miller (rmiller@mozilla.com)
#   Mike Trinkala (trink@mozilla.com)
#   Ben Bangert (bbangert@mozilla.com)
#
# ***** END LICENSE BLOCK *****/

package pipeline

import (
	"errors"
	"fmt"
	"github.com/mozilla-services/heka/client"
	"github.com/mozilla-services/heka/message"
	"log"
	"sync"
	"time"
)

var ErrUnknownPluginType = errors.New("Unable to assert this is an Output or Filter")

// Base interface for the Heka plugin runners.
type PluginRunner interface {
	// All plugins either can or cannot stop without causing Heka to shutdown.
	Stoppable

	// Plugin name.
	Name() string

	// Plugin name mutator.
	SetName(name string)

	// Underlying plugin object.
	Plugin() Plugin

	// Plugins should call `LogError` on their runner to log error messages
	// rather than doing logging directly.
	LogError(err error)

	// Plugins should call `LogMessage` on their runner to write to the log
	// rather than doing so directly.
	LogMessage(msg string)

	// Sets the amount of currently 'leaked' packs that have gone through
	// this plugin. The new value will overwrite prior ones.
	SetLeakCount(count int)

	// Returns the current leak count
	LeakCount() int
}

// Base struct for the specialized PluginRunners
type pRunnerBase struct {
	notStoppable
	name      string
	plugin    Plugin
	h         PluginHelper
	leakCount int
	maker     PluginMaker
}

func (pr *pRunnerBase) Name() string {
	return pr.name
}

func (pr *pRunnerBase) SetName(name string) {
	pr.name = name
}

func (pr *pRunnerBase) Plugin() Plugin {
	return pr.plugin
}

func (pr *pRunnerBase) SetLeakCount(count int) {
	pr.leakCount = count
}

func (pr *pRunnerBase) LeakCount() int {
	return pr.leakCount
}

// Heka PluginRunner for Input plugins.
type InputRunner interface {
	PluginRunner
	// Input channel from which Inputs can get fresh PipelinePacks, ready to
	// be populated.
	InChan() chan *PipelinePack
	// Associated Input plugin object.
	Input() Input
	// Returns a ticker channel configured to send ticks at an interval
	// specified by the plugin's ticker_interval config value, if provided.
	Ticker() (ticker <-chan time.Time)
	// Starts Input in a separate goroutine and returns. Should decrement the
	// plugin when the Input stops and the goroutine has completed.
	Start(h PluginHelper, wg *sync.WaitGroup) (err error)
	// Injects PipelinePack into the Heka Router's input channel for delivery
	// to all Filter and Output plugins with corresponding message_matchers.
	Inject(pack *PipelinePack)
	// If Transient returns true, Heka won't try to keep the Input running,
	// nor will it generate reporting data. Life span and reporting for a
	// transient InputRunner must be managed by the code that creates the
	// runner.
	Transient() bool
}

type iRunner struct {
	pRunnerBase
	input     Input
	config    CommonInputConfig
	inChan    chan *PipelinePack
	ticker    <-chan time.Time
	transient bool
}

func (ir *iRunner) Ticker() (ticker <-chan time.Time) {
	return ir.ticker
}

func (ir *iRunner) Transient() bool {
	return ir.transient
}

// Creates and returns a new (not yet started) InputRunner associated w/ the
// provided Input. If transient is true Heka won't try to manage the input's
// life span at all, it's up to the caller to do so.
func NewInputRunner(name string, input Input, config CommonInputConfig,
	transient bool) (ir InputRunner) {

	runner := &iRunner{
		pRunnerBase: pRunnerBase{
			name:   name,
			plugin: input.(Plugin),
		},
		input:     input,
		config:    config,
		transient: transient,
	}
	return runner
}

func (ir *iRunner) Input() Input {
	return ir.plugin.(Input)
}

func (ir *iRunner) InChan() chan *PipelinePack {
	return ir.inChan
}

func (ir *iRunner) Start(h PluginHelper, wg *sync.WaitGroup) (err error) {
	ir.h = h
	ir.inChan = h.PipelineConfig().inputRecycleChan

	if ir.config.Ticker != 0 {
		tickLength := time.Duration(ir.config.Ticker) * time.Second
		ir.ticker = time.Tick(tickLength)
	}

	go ir.Starter(h, wg)
	return
}

func (ir *iRunner) Starter(h PluginHelper, wg *sync.WaitGroup) {
	defer wg.Done()

	pConfig := h.PipelineConfig()
	globals := pConfig.Globals
	rh, err := NewRetryHelper(ir.config.Retries)
	if err != nil {
		ir.LogError(err)
		globals.ShutDown()
		return
	}

	for !globals.IsShuttingDown() {
		// ir.Input().Run() shouldn't return unless error or shutdown.
		if err := ir.input.Run(ir, h); err != nil {
			ir.LogError(err)
		} else if !ir.transient {
			rh.Reset()
			ir.LogMessage("stopped")
		}

		// Are we supposed to stop? Save ourselves some time by exiting now.
		if globals.IsShuttingDown() {
			break
		}

		// If we're transient we just exit.
		if ir.transient {
			break
		}
		// We're not transient, we either try to restart or we shut down
		// altogether.
		recon, ok := ir.plugin.(Restarting)
		if !ok {
			ir.LogMessage("has stopped, shutting down.")
			globals.ShutDown()
			break
		}

		recon.CleanupForRestart()
		if ir.maker == nil {
			ir.maker = pConfig.makers["Input"][ir.name]
		}
	initLoop:
		if err = rh.Wait(); err != nil {
			// We've used up our retry attempts, exit.
			ir.LogError(err)
			globals.ShutDown()
			break
		}
		ir.LogMessage("exited, now restarting.")
		if err = ir.plugin.Init(ir.maker.Config()); err != nil {
			ir.LogError(err)
			goto initLoop
		}
	}
}

func (ir *iRunner) Inject(pack *PipelinePack) {
	ir.h.PipelineConfig().router.InChan() <- pack
}

func (ir *iRunner) LogError(err error) {
	log.Printf("Input '%s' error: %s", ir.name, err)
}

func (ir *iRunner) LogMessage(msg string) {
	log.Printf("Input '%s': %s", ir.name, msg)
}

// Heka PluginRunner for Decoder plugins. Decoding is typically a simpler job,
// so these runners handle a bit more than the others.
type DecoderRunner interface {
	PluginRunner
	// Returns associated Decoder plugin object.
	Decoder() Decoder
	// Starts the DecoderRunner so it's listening for incoming PipelinePacks.
	// Should decrement the wait group after shut down has completed.
	Start(h PluginHelper, wg *sync.WaitGroup)
	// Returns the channel into which incoming PipelinePacks to be decoded
	// should be dropped.
	InChan() chan *PipelinePack
	// Returns the running Heka router for direct use by decoder plugins.
	Router() MessageRouter
	// Fetches a new pack from the input supply and returns it to the caller,
	// for decoders that generate multiple messages from a single input
	// message.
	NewPack() *PipelinePack
}

type dRunner struct {
	pRunnerBase
	inChan chan *PipelinePack
	router *messageRouter
	h      PluginHelper
}

// Creates and returns a new (but not yet started) DecoderRunner for the
// provided Decoder plugin.
func NewDecoderRunner(name string, decoder Decoder, chanSize int) DecoderRunner {
	return &dRunner{
		pRunnerBase: pRunnerBase{
			name:   name,
			plugin: decoder.(Plugin),
		},
		inChan: make(chan *PipelinePack, chanSize),
	}
}

func (dr *dRunner) Decoder() Decoder {
	return dr.plugin.(Decoder)
}

func (dr *dRunner) Start(h PluginHelper, wg *sync.WaitGroup) {
	dr.h = h
	dr.router = h.PipelineConfig().router
	go func() {
		var (
			pack  *PipelinePack
			packs []*PipelinePack
			err   error
		)
		if wanter, ok := dr.Decoder().(WantsDecoderRunner); ok {
			wanter.SetDecoderRunner(dr)
		}
		for pack = range dr.inChan {
			if packs, err = dr.Decoder().Decode(pack); packs != nil {
				for _, p := range packs {
					h.PipelineConfig().router.InChan() <- p
				}
			} else {
				if err != nil {
					dr.LogError(err)
				}
				pack.Recycle()
				continue
			}
		}
		if wanter, ok := dr.Decoder().(WantsDecoderRunnerShutdown); ok {
			wanter.Shutdown()
		}
		dr.LogMessage("stopped")
		wg.Done()
	}()
}

func (dr *dRunner) InChan() chan *PipelinePack {
	return dr.inChan
}

func (dr *dRunner) Router() MessageRouter {
	return dr.router
}

func (dr *dRunner) NewPack() *PipelinePack {
	return <-dr.h.PipelineConfig().inputRecycleChan
}

func (dr *dRunner) LogError(err error) {
	log.Printf("Decoder '%s' error: %s", dr.name, err)
}

func (dr *dRunner) LogMessage(msg string) {
	log.Printf("Decoder '%s': %s", dr.name, msg)
}

// Any decoder that needs access to its DecoderRunner can implement this
// interface and it will be provided at DecoderRunner start time.
type WantsDecoderRunner interface {
	SetDecoderRunner(dr DecoderRunner)
}

// Any decoder that needs to know when the DecoderRunner is exiting can
// implement this interface and it will called on DecoderRunner exit.
type WantsDecoderRunnerShutdown interface {
	Shutdown()
}

// Heka PluginRunner interface for Filter type plugins.
type FilterRunner interface {
	PluginRunner
	// Input channel on which the Filter should listen for incoming messages
	// to be processed. Closure of the channel signals shutdown to the filter.
	InChan() chan *PipelinePack
	// Associated Filter plugin object.
	Filter() Filter
	// Starts the Filter (so it's listening on the input channel for messages
	// to be processed) in a separate goroutine and returns. Should decrement
	// the wait group when the Filter has stopped and the goroutine has
	// completed.
	Start(h PluginHelper, wg *sync.WaitGroup) (err error)
	// Returns a ticker channel configured to send ticks at an interval
	// specified by the plugin's ticker_interval config value, if provided.
	Ticker() (ticker <-chan time.Time)
	// Hands provided PipelinePack to the Heka Router for delivery to any
	// Filter or Output plugins with a corresponding message_matcher. Returns
	// false and doesn't perform message injection if the message would be
	// caught by the sending Filter's message_matcher.
	Inject(pack *PipelinePack) bool
	// Parsing engine for this Filter's message_matcher.
	MatchRunner() *MatchRunner
	// Retains a pack for future delivery to the plugin when a plugin needs to
	// shut down and wants to retain the pack for the next time its running
	// properly.
	RetainPack(pack *PipelinePack)
}

// Heka PluginRunner for Output plugins.
type OutputRunner interface {
	PluginRunner
	// Input channel where Output should be listening for incoming messages.
	InChan() chan *PipelinePack
	// Associated Output plugin instance.
	Output() Output
	// Starts the Output plugin listening on the input channel in a separate
	// goroutine and returns. Wait group should be released when the Output
	// plugin shuts down cleanly and the goroutine has completed.
	Start(h PluginHelper, wg *sync.WaitGroup) (err error)
	// Returns a ticker channel configured to send ticks at an interval
	// specified by the plugin's ticker_interval config value, if provided.
	Ticker() (ticker <-chan time.Time)
	// Retains a pack for future delivery to the plugin when a plugin needs
	// to shut down and wants to retain the pack for the next time its
	// running properly
	RetainPack(pack *PipelinePack)
	// Parsing engine for this Output's message_matcher.
	MatchRunner() *MatchRunner
	// Returns an instance of the Encoder specified by the output's config, or
	// nil if none was specified. Multiple calls will return the same
	// instance.
	Encoder() Encoder
	// Uses the output's Encoder to encode the message attached to the
	// provided PipelinePack. Will prepend a Heka stream framing header if
	// use_framing was set to true in the output configuration.
	Encode(pack *PipelinePack) (output []byte, err error)
	// Returns whether or not use_framing was set to true in the output's
	// configuration, i.e. whether or not Heka stream framing will be applied
	// to the results of calls to the Encode method.
	UsesFraming() bool
	// Allows an output to specify whether or not it's using framing.
	SetUseFraming(useFraming bool)
}

type foRunnerKind int

const (
	foUnknown foRunnerKind = iota
	foFilter
	foOutput
)

func (kind foRunnerKind) String() string {
	switch kind {
	case foFilter:
		return "filter"
	case foOutput:
		return "output"
	}
	return "unknown"
}

// This one struct provides the implementation of both FilterRunner and
// OutputRunner interfaces.
type foRunner struct {
	pRunnerBase
	pluginType string
	config     CommonFOConfig
	matcher    *MatchRunner
	ticker     <-chan time.Time
	inChan     chan *PipelinePack
	h          PluginHelper
	retainPack *PipelinePack
	leakCount  int
	encoder    Encoder // output only
	useFraming bool    // output only
	canExit    bool
	kind       foRunnerKind
	pConfig    *PipelineConfig
	lastErr    error
}

// Creates and returns foRunner pointer for use as either a FilterRunner or an
// OutputRunner.
func NewFORunner(name string, plugin Plugin, config CommonFOConfig,
	pluginType string, chanSize int) (*foRunner, error) {

	runner := &foRunner{
		pRunnerBase: pRunnerBase{
			name:   name,
			plugin: plugin,
		},
		pluginType: pluginType,
		config:     config,
	}
	runner.inChan = make(chan *PipelinePack, chanSize)

	if config.Matcher == "" {
		return nil, fmt.Errorf("'%s' missing message matcher", name)
	}

	matcher, err := NewMatchRunner(config.Matcher, config.Signer, runner, chanSize)
	if err != nil {
		return nil, fmt.Errorf("Can't create message matcher for '%s': %s", name, err)
	}
	runner.matcher = matcher

	if config.UseFraming != nil && *config.UseFraming {
		runner.useFraming = true
	}

	if _, ok := plugin.(Filter); ok {
		runner.kind = foFilter
	} else if _, ok := plugin.(Output); ok {
		runner.kind = foOutput
	} else {
		err := fmt.Errorf("FORunner plugin must be filter or output, %s is neither",
			name)
		return nil, err
	}

	return runner, nil
}

func (foRunner *foRunner) Start(h PluginHelper, wg *sync.WaitGroup) (err error) {
	foRunner.h = h
	foRunner.pConfig = h.PipelineConfig()

	if foRunner.pluginType == "SandboxFilter" {
		// No maker means we're a dynamic filter and we can exit.
		maker := foRunner.pConfig.makers["Filter"][foRunner.name]
		if maker == nil {
			foRunner.canExit = true
		}
	}

	if foRunner.config.Ticker != 0 {
		tickLength := time.Duration(foRunner.config.Ticker) * time.Second
		foRunner.ticker = time.Tick(tickLength)
	}

	if foRunner.config.Encoder != "" {
		fullName := fmt.Sprintf("%s-%s", foRunner.name, foRunner.config.Encoder)
		encoder, ok := foRunner.pConfig.Encoder(foRunner.config.Encoder, fullName)
		if !ok {
			return fmt.Errorf("%s can't create encoder %s", foRunner.name,
				foRunner.config.Encoder)
		}
		foRunner.encoder = encoder
	}

	if foRunner.matcher != nil {
		switch foRunner.kind {
		case foFilter:
			foRunner.pConfig.router.fMatcherMap[foRunner.name] = foRunner.matcher
		case foOutput:
			foRunner.pConfig.router.oMatcherMap[foRunner.name] = foRunner.matcher
		}
	}

	go foRunner.Starter(h, wg)
	return
}

func (foRunner *foRunner) IsStoppable() bool {
	return foRunner.canExit
}

func (foRunner *foRunner) Unregister(pConfig *PipelineConfig) error {
	switch foRunner.kind {
	case foFilter:
		go pConfig.RemoveFilterRunner(foRunner.Name())
	case foOutput:
		go pConfig.RemoveOutputRunner(foRunner)
	}
	for pack := range foRunner.inChan {
		pack.Recycle()
	}
	return nil
}

func (foRunner *foRunner) exit() {
	// Just exit if we're stopping.
	if foRunner.pConfig.Globals.IsShuttingDown() {
		return
	}

	// Also, if this isn't a "stoppable" plugin we shut everything down.
	if !foRunner.IsStoppable() {
		foRunner.LogMessage("has stopped, shutting down.")
		foRunner.pConfig.Globals.ShutDown()
		return
	}

	// If we're stoppable we unregister the plugin and, if necessary, send a
	// termination message.
	foRunner.LogMessage("has stopped, exiting plugin without shutting down.")
	foRunner.Unregister(foRunner.pConfig)

	// A TerminatedError means the plugin was terminated, and has generated
	// its own termination message, so we can return now.
	if _, ok := foRunner.lastErr.(TerminatedError); ok {
		return
	}

	pack := foRunner.pConfig.PipelinePack(0)
	pack.Message.SetType("heka.terminated")
	pack.Message.SetLogger(HEKA_DAEMON)
	message.NewStringField(pack.Message, "plugin", foRunner.name)

	var errMsg string
	if foRunner.lastErr != nil {
		errMsg = foRunner.lastErr.Error()
	} else if foRunner.pluginType == "SandboxFilter" {
		errMsg = "Filter unloaded."
	} else {
		// This is simply when run returns no errors, but the plugin has
		// exited anyway.
		errMsg = "Run exited."
	}
	errMsg = "Error: " + errMsg

	payload := fmt.Sprintf("%s (type %s) terminated. %s", foRunner.name,
		foRunner.pluginType, errMsg)
	pack.Message.SetPayload(payload)
	// Do not call the Inject method b/c we might get burned by the explicit
	// message matcher check in cases where it looks like we'd be injecting a
	// message to ourself.
	foRunner.pConfig.router.inChan <- pack
}

func (foRunner *foRunner) Starter(helper PluginHelper, wg *sync.WaitGroup) {
	defer wg.Done()

	var err error
	globals := foRunner.pConfig.Globals

	rh, err := NewRetryHelper(foRunner.config.Retries)
	if err != nil {
		foRunner.LogError(err)
		globals.ShutDown()
		return
	}

	if foRunner.matcher != nil {
		sampleDenom := globals.SampleDenominator
		foRunner.matcher.Start(foRunner.inChan, sampleDenom)
	}

	// Handle the cleanup
	defer foRunner.exit()

	for !globals.IsShuttingDown() {
		// `Run` method only returns if there's an error or we're shutting
		// down.
		switch foRunner.kind {
		case foFilter:
			filter := foRunner.Filter()
			err = filter.Run(foRunner, helper)
		case foOutput:
			output := foRunner.Output()
			err = output.Run(foRunner, helper)
		}

		if err == nil {
			rh.Reset()
		} else {
			// Keep track of all the errors for later
			foRunner.lastErr = err
			foRunner.LogError(err)
		}

		foRunner.LogMessage("stopped")

		// Are we supposed to stop? Save ourselves some time by exiting now.
		if globals.IsShuttingDown() {
			break
		}

		// We stop and let this quit if its not a restarting plugin.
		recon, ok := foRunner.plugin.(Restarting)
		if !ok {
			break
		}
		recon.CleanupForRestart()
		if foRunner.maker == nil {
			var makers map[string]PluginMaker
			switch foRunner.kind {
			case foFilter:
				makers = foRunner.pConfig.makers["Filter"]
			case foOutput:
				makers = foRunner.pConfig.makers["Output"]
			}
			foRunner.maker = makers[foRunner.name]
		}
	initLoop:
		if err = rh.Wait(); err != nil {
			// An error means we've used up our retry attempts, so we
			// exit.
			foRunner.lastErr = err
			foRunner.LogError(err)
			break
		}
		foRunner.LogMessage("now restarting")
		if err = foRunner.plugin.Init(foRunner.maker.Config()); err != nil {
			foRunner.LogError(err)
			goto initLoop
		}
	}
}

func (foRunner *foRunner) Inject(pack *PipelinePack) bool {
	spec := foRunner.MatchRunner().MatcherSpecification()
	match := spec.Match(pack.Message)
	if match {
		pack.Recycle()
		foRunner.LogError(fmt.Errorf("attempted to Inject a message to itself"))
		return false
	}
	// Do the actual injection in a separate goroutine so we free up the
	// caller; this prevents deadlocks when the caller's InChan is backed up,
	// backing up the router, which would block us here.
	go func() {
		foRunner.h.PipelineConfig().router.InChan() <- pack
	}()
	return true
}

func (foRunner *foRunner) LogError(err error) {
	log.Printf("Plugin '%s' error: %s", foRunner.name, err)
}

func (foRunner *foRunner) LogMessage(msg string) {
	log.Printf("Plugin '%s': %s", foRunner.name, msg)
}

func (foRunner *foRunner) Ticker() (ticker <-chan time.Time) {
	return foRunner.ticker
}

func (foRunner *foRunner) RetainPack(pack *PipelinePack) {
	foRunner.retainPack = pack
}

func (foRunner *foRunner) InChan() (inChan chan *PipelinePack) {
	if foRunner.retainPack != nil {
		retainChan := make(chan *PipelinePack)

		go func(pack *PipelinePack) {
			retainChan <- pack
			close(retainChan)
		}(foRunner.retainPack)

		foRunner.retainPack = nil
		return retainChan
	}
	return foRunner.inChan
}

func (foRunner *foRunner) MatchRunner() *MatchRunner {
	return foRunner.matcher
}

func (foRunner *foRunner) SetMatchRunner(mr *MatchRunner) {
	foRunner.matcher = mr
}

func (foRunner *foRunner) Output() Output {
	return foRunner.plugin.(Output)
}

func (foRunner *foRunner) Filter() Filter {
	return foRunner.plugin.(Filter)
}

func (foRunner *foRunner) Encoder() Encoder {
	return foRunner.encoder
}

func (foRunner *foRunner) Encode(pack *PipelinePack) (output []byte, err error) {
	var encoded []byte
	if encoded, err = foRunner.encoder.Encode(pack); err != nil {
		return
	}
	if foRunner.useFraming {
		client.CreateHekaStream(encoded, &output, nil)
	} else {
		output = encoded
	}
	return
}

func (foRunner *foRunner) UsesFraming() bool {
	return foRunner.useFraming
}

func (foRunner *foRunner) SetUseFraming(useFraming bool) {
	foRunner.useFraming = useFraming
}
