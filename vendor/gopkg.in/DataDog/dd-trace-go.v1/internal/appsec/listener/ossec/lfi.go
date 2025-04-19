// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2024 Datadog, Inc.

package ossec

import (
	"os"

	"gopkg.in/DataDog/dd-trace-go.v1/appsec/events"
	"gopkg.in/DataDog/dd-trace-go.v1/internal/appsec/config"
	"gopkg.in/DataDog/dd-trace-go.v1/internal/appsec/dyngo"
	"gopkg.in/DataDog/dd-trace-go.v1/internal/appsec/emitter/ossec"
	"gopkg.in/DataDog/dd-trace-go.v1/internal/appsec/emitter/waf"
	"gopkg.in/DataDog/dd-trace-go.v1/internal/appsec/emitter/waf/addresses"
	"gopkg.in/DataDog/dd-trace-go.v1/internal/appsec/listener"
)

type Feature struct{}

func (*Feature) String() string {
	return "LFI Protection"
}

func (*Feature) Stop() {}

func NewOSSecFeature(cfg *config.Config, rootOp dyngo.Operation) (listener.Feature, error) {
	if !cfg.RASP || !cfg.SupportedAddresses.AnyOf(addresses.ServerIOFSFileAddr) {
		return nil, nil
	}

	feature := &Feature{}
	dyngo.On(rootOp, feature.OnStart)
	return feature, nil
}

func (*Feature) OnStart(op *ossec.OpenOperation, args ossec.OpenOperationArgs) {
	dyngo.OnData(op, func(err *events.BlockingSecurityEvent) {
		dyngo.OnFinish(op, func(_ *ossec.OpenOperation, res ossec.OpenOperationRes[*os.File]) {
			if res.Err != nil {
				*res.Err = err
			}
		})
	})

	dyngo.EmitData(op, waf.RunEvent{
		Operation: op,
		RunAddressData: addresses.NewAddressesBuilder().
			WithFilePath(args.Path).
			Build(),
	})
}
