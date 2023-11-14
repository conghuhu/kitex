/*
 * Copyright 2021 CloudWeGo Authors
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package circuitbreak

import (
	"context"
	"errors"
	"sync/atomic"
	"time"

	"github.com/bytedance/gopkg/cloud/circuitbreaker"

	"github.com/cloudwego/kitex/pkg/endpoint"
	"github.com/cloudwego/kitex/pkg/kerrors"
	"github.com/cloudwego/kitex/pkg/utils"
)

// Parameter contains parameters for circuit breaker.
type Parameter struct {
	// Enabled means if to enable the circuit breaker.
	Enabled bool
	// ErrorRate means the rate at which breaks.
	ErrorRate float64
	// MinimalSample means the minimal sample need before break.
	MinimalSample int64
}

// ErrorType means the error type.
type ErrorType int

// Constants for ErrorType.
const (
	// TypeIgnorable means ignorable error, which is ignored by the circuit breaker.
	TypeIgnorable ErrorType = iota
	// TypeTimeout means timeout error.
	TypeTimeout
	// TypeFailure means the request failed, but it isn't timeout.
	TypeFailure
	// TypeSuccess means the request successes.
	TypeSuccess
)

// IsError returns whether the errType could cause a circuit breaker to open.
func IsError(errType ErrorType) bool {
	return errType == TypeTimeout || errType == TypeFailure
}

// WrapErrorWithType is used to define the ErrorType for CircuitBreaker.
// If you don't want the error trigger fuse, you can set the ErrorType to TypeIgnorable,
// the error won't be regarded as failed.
// eg: return circuitbreak.WrapErrorWithType.WithCause(err, circuitbreak.TypeIgnorable) in customized middleware.
func WrapErrorWithType(err error, errorType ErrorType) CircuitBreakerAwareError {
	return &errorWrapperWithType{err: err, errType: errorType}
}

// Control is the control strategy of the circuit breaker.
type Control struct {
	// Implement this to generate a key for the circuit breaker panel.
	GetKey func(ctx context.Context, request interface{}) (key string, enabled bool)

	// Implement this to determine the type of error.
	GetErrorType func(ctx context.Context, request, response interface{}, err error) ErrorType

	// Implement this to provide more detailed information about the circuit breaker.
	// The err argument is always a kerrors.ErrCircuitBreak.
	DecorateError func(ctx context.Context, request interface{}, err error) error
}

const cbTickDuration = 1 * time.Second

var sharedTicker = utils.NewSharedTicker(cbTickDuration)

type cbTicker struct {
	panel   circuitbreaker.Panel
	hasOpen *int32
}

func (t *cbTicker) Tick() {
	var hasOpen bool
	for _, breaker := range t.panel.DumpBreakers() {
		if breaker.State() != circuitbreaker.Closed ||
			breaker.Metricer().Failures() > 0 || breaker.Metricer().Timeouts() > 0 {
			hasOpen = true
			break
		}
	}
	if !hasOpen {
		atomic.StoreInt32(t.hasOpen, 0)
		sharedTicker.Delete(t)
	}
}

// NewCircuitBreakerMW creates a circuit breaker MW using the given Control strategy and Panel.
func NewCircuitBreakerMW(control Control, panel circuitbreaker.Panel) endpoint.Middleware {
	var hasOpen int32
	return func(next endpoint.Endpoint) endpoint.Endpoint {
		return func(ctx context.Context, request, response interface{}) (err error) {
			// If circuit breaker is not enabled, just bypass it.
			if atomic.LoadInt32(&hasOpen) == 0 {
				err = next(ctx, request, response)
				// Disable the bypass circuit breaker feature when encountering an error.
				if isErr := err != nil && IsError(control.GetErrorType(ctx, request, response, err)); isErr ||
					atomic.LoadInt32(&hasOpen) != 0 {
					key, enabled := control.GetKey(ctx, request)
					if enabled {
						if isErr && atomic.CompareAndSwapInt32(&hasOpen, 0, 1) {
							// Start a ticker to asynchronously judge whether the circuit breaker has been closed.
							sharedTicker.Add(&cbTicker{
								panel:   panel,
								hasOpen: &hasOpen,
							})
						}
						RecordStat(ctx, request, response, err, key, &control, panel)
					}
				}
				return
			}
			key, enabled := control.GetKey(ctx, request)
			if !enabled {
				return next(ctx, request, response)
			}

			if !panel.IsAllowed(key) {
				return control.DecorateError(ctx, request, kerrors.ErrCircuitBreak)
			}

			err = next(ctx, request, response)
			RecordStat(ctx, request, response, err, key, &control, panel)
			return
		}
	}
}

// RecordStat to report request result to circuit breaker
func RecordStat(ctx context.Context, request, response interface{}, err error, cbKey string, ctl *Control, panel circuitbreaker.Panel) {
	switch ctl.GetErrorType(ctx, request, response, err) {
	case TypeTimeout:
		panel.Timeout(cbKey)
	case TypeFailure:
		panel.Fail(cbKey)
	case TypeSuccess:
		panel.Succeed(cbKey)
	}
}

// CircuitBreakerAwareError is used to wrap ErrorType
type CircuitBreakerAwareError interface {
	error
	TypeForCircuitBreaker() ErrorType
}

type errorWrapperWithType struct {
	errType ErrorType
	err     error
}

func (e errorWrapperWithType) TypeForCircuitBreaker() ErrorType {
	return e.errType
}

func (e errorWrapperWithType) Error() string {
	return e.err.Error()
}

func (e errorWrapperWithType) Unwrap() error {
	return e.err
}

func (e errorWrapperWithType) Is(target error) bool {
	return errors.Is(e.err, target)
}
