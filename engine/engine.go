// Copyright 2021 Optakt Labs OÜ
//
// Licensed under the Apache License, Version 2.0 (the "License"); you may not
// use this file except in compliance with the License. You may obtain a copy of
// the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS, WITHOUT
// WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied. See the
// License for the specific language governing permissions and limitations under
// the License.

package engine

import (
	"os"

	"github.com/rs/zerolog"
)

// Engine is an engine that is composed of multiple components.
type Engine struct {
	log        zerolog.Logger
	components []*component

	stop   chan os.Signal
	notify chan error
}

// New creates a new engine.
func New(log zerolog.Logger, name string, stop chan os.Signal) *Engine {
	e := Engine{
		log:  log.With().Str("engine", name).Logger(),
		stop: stop,
	}

	return &e
}

// Component registers a new component for the engine. Components will be shut down
// in the same order as the one in which they were registered.
func (e *Engine) Component(name string, run func() error, stop func()) *Engine {
	c := component{
		log:  e.log.With().Str("component", name).Logger(),
		run:  run,
		stop: stop,
	}

	e.components = append(e.components, &c)

	return e
}

// Run launches the engine components and waits for them to either finish successfully,
// fail, or for an external signal to shut the engine down.
func (e *Engine) Run() error {
	e.notify = make(chan error, len(e.components))
	for _, component := range e.components {
		go component.Run(e.notify)
	}

	// Here, we are waiting for a signal, or for one of the components to fail
	// or finish. In both cases, we proceed to shut down everything, while also
	// entering a goroutine that allows us to force shut down by sending
	// another signal.
	select {
	case <-e.stop:
		e.log.Info().Msg("engine stopping")
	case err := <-e.notify:
		if err != nil {
			e.log.Error().Err(err).Msg("engine failed")
			e.Stop()
			return err
		}
		e.log.Info().Msg("engine done")
	}
	go func() {
		<-e.stop
		e.log.Warn().Msg("forcing exit")
		os.Exit(1)
	}()

	return nil
}

// Stop stops each of the engine's components one by one, in the order in which they were
// registered.
func (e *Engine) Stop() {
	// Components are stopped in the reverse order in which they were registered.
	for i := len(e.components) - 1; i >= 0; i-- {
		e.components[i].Stop()
	}
}
