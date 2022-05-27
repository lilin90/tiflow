// Copyright 2022 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package manager

import (
	"context"
	"time"

	"github.com/pingcap/errors"
	"github.com/pingcap/log"
	"go.uber.org/zap"

	"github.com/pingcap/tiflow/engine/pkg/clock"
	resModel "github.com/pingcap/tiflow/engine/pkg/externalresource/resourcemeta/model"
	pkgOrm "github.com/pingcap/tiflow/engine/pkg/orm"
)

const (
	gcCheckInterval = 10 * time.Second
	gcTimeout       = 10 * time.Second
)

type gcHandlerFunc = func(ctx context.Context, meta *resModel.ResourceMeta) error

// DefaultGCRunner implements GCRunner.
type DefaultGCRunner struct {
	client     pkgOrm.ResourceClient
	gcHandlers map[resModel.ResourceType]gcHandlerFunc
	notifyCh   chan struct{}

	clock clock.Clock
}

// NewGCRunner returns a new GCRunner.
func NewGCRunner(
	client pkgOrm.ResourceClient,
	gcHandlers map[resModel.ResourceType]gcHandlerFunc,
) *DefaultGCRunner {
	return &DefaultGCRunner{
		client:     client,
		gcHandlers: gcHandlers,
		notifyCh:   make(chan struct{}, 1),
		clock:      clock.New(),
	}
}

// Run runs the GCRunner. It blocks until ctx is canceled.
func (r *DefaultGCRunner) Run(ctx context.Context) error {
	// TODO this will result in DB queries every 10 seconds.
	// This is a very naive strategy, we will modify the
	// algorithm after doing enough system testing.
	ticker := r.clock.Ticker(gcCheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return errors.Trace(ctx.Err())
		case <-ticker.C:
		case <-r.notifyCh:
		}

		timeoutCtx, cancel := context.WithTimeout(ctx, gcTimeout)
		err := r.gcOnce(timeoutCtx)
		cancel()

		if err != nil {
			log.L().Warn("resource GC encountered error", zap.Error(err))
		}
	}
}

// Notify is used to ask GCRunner to GC the next resource immediately.
// It is used when we have just marked a resource as gc_pending.
func (r *DefaultGCRunner) Notify() {
	select {
	case r.notifyCh <- struct{}{}:
	default:
	}
}

func (r *DefaultGCRunner) gcOnce(
	ctx context.Context,
) error {
	res, err := r.client.GetOneResourceForGC(ctx)
	if pkgOrm.IsNotFoundError(err) {
		// It is expected that sometimes we have
		// nothing to GC.
		return nil
	}
	if err != nil {
		return err
	}

	log.Info("start gc'ing resource", zap.Any("resource", res))
	if !res.GCPending {
		log.L().Panic("unexpected gc_pending = false")
	}

	tp, _, err := resModel.ParseResourcePath(res.ID)
	if err != nil {
		return err
	}

	handler, exists := r.gcHandlers[tp]
	if !exists {
		log.L().Warn("no gc handler is found for given resource type",
			zap.Any("resource-id", res.ID))
		// Return nil here for potential backward compatibility when we do
		// rolling upgrades online.
		return nil
	}

	if err := handler(ctx, res); err != nil {
		return err
	}

	result, err := r.client.DeleteResource(ctx, res.ID)
	if err != nil {
		// If deletion fails, we do not need to retry for now.
		log.L().Warn("Failed to delete resource after GC", zap.Any("resource", res))
		return nil
	}
	if result.RowsAffected() == 0 {
		log.L().Warn("Resource is deleted unexpectedly", zap.Any("resource", res))
	}

	return nil
}