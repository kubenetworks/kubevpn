// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2023 Datadog, Inc.

package hostname

import (
	"context"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"gopkg.in/DataDog/dd-trace-go.v1/internal/hostname/azure"
	"gopkg.in/DataDog/dd-trace-go.v1/internal/hostname/ec2"
	"gopkg.in/DataDog/dd-trace-go.v1/internal/hostname/ecs"
	"gopkg.in/DataDog/dd-trace-go.v1/internal/hostname/gce"
	"gopkg.in/DataDog/dd-trace-go.v1/internal/hostname/validate"
	"gopkg.in/DataDog/dd-trace-go.v1/internal/log"
)

// For testing purposes
var (
	fargatePf = fargate
)

var (
	cachedHostname  string
	cachedAt        time.Time
	cachedProvider  string
	cacheExpiration = 5 * time.Minute
	m               sync.RWMutex
	isRefreshing    atomic.Value
)

const fargateName = "fargate"

func init() {
	isRefreshing.Store(false)
}

// getCached returns the cached hostname, cached provider and a bool indicating if the hostname has expired
func getCached(now time.Time) (string, string, bool) {
	m.RLock()
	defer m.RUnlock()
	if now.Sub(cachedAt) > cacheExpiration {
		return cachedHostname, cachedProvider, true
	}
	return cachedHostname, cachedProvider, false
}

// setCached caches the newHostname
func setCached(now time.Time, newHostname string, newProvider string) {
	m.Lock()
	defer m.Unlock()
	cachedHostname = newHostname
	cachedAt = now
	cachedProvider = newProvider
}

type provider struct {
	name string
	// Should we stop going down the list of providers if this one is successful
	stopIfSuccessful bool
	pf               providerFetch
}

type providerFetch func(ctx context.Context, currentHostname string) (string, error)

var providerCatalog = []provider{
	{
		name:             "configuration",
		stopIfSuccessful: true,
		pf:               fromConfig,
	},
	{
		name:             fargateName,
		stopIfSuccessful: true,
		pf:               fromFargate,
	},
	{
		name:             "gce",
		stopIfSuccessful: true,
		pf:               fromGce,
	},
	{
		name:             "azure",
		stopIfSuccessful: true,
		pf:               fromAzure,
	},
	// The following providers are coupled. Their behavior changes depending on the result of the previous provider.
	// Therefore, 'stopIfSuccessful' is set to false.
	{
		name:             "fqdn",
		stopIfSuccessful: false,
		pf:               fromFQDN,
	},
	{
		name:             "container",
		stopIfSuccessful: false,
		pf:               fromContainer,
	},
	{
		name:             "os",
		stopIfSuccessful: false,
		pf:               fromOS,
	},
	{
		name:             "aws",
		stopIfSuccessful: false,
		pf:               fromEC2,
	},
}

// Get returns the cached hostname for the tracer, empty if we haven't found one yet.
// Spawning a go routine to update the hostname if it is empty or out of date
func Get() string {
	now := time.Now()
	var (
		ch      string
		expired bool
		pv      string
	)
	// if provider is fargate never refresh
	// Otherwise, refresh on expiration or if hostname hasn't been found.
	if ch, pv, expired = getCached(now); pv == fargateName || (!expired && ch != "") {
		return ch
	}
	// Use CAS to avoid spawning more than one go-routine trying to update the cached hostname
	ir := isRefreshing.CompareAndSwap(false, true)
	if ir {
		// TODO: One optimization we could do here is hook into the tracer shutdown signal to gracefully disconnect here
		// For now, we think the added complexity isn't worth it for this single go routine that only runs every 5 minutes.
		go func() {
			updateHostname(now)
		}()
	}
	return ch
}

func updateHostname(now time.Time) {
	defer isRefreshing.Store(false)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	var hostname string
	var hnProvider string

	for _, p := range providerCatalog {
		detectedHostname, err := p.pf(ctx, hostname)
		if err != nil {
			log.Debug("Unable to get hostname from provider %s: %v", p.name, err)
			continue
		}
		hostname = detectedHostname
		hnProvider = p.name
		log.Debug("Found hostname %s, from provider %s", hostname, p.name)
		if p.stopIfSuccessful {
			log.Debug("Hostname detection stopping early")
			setCached(now, hostname, p.name)
			return
		}
	}
	if hostname != "" {
		log.Debug("Winning hostname %s from provider %s", hostname, hnProvider)
		setCached(now, hostname, hnProvider)
	} else {
		log.Debug("Unable to reliably determine hostname. You can define one via env var DD_HOSTNAME")
	}
}

func fromConfig(_ context.Context, _ string) (string, error) {
	hn := os.Getenv("DD_HOSTNAME")
	err := validate.ValidHostname(hn)
	if err != nil {
		return "", err
	}
	return hn, nil
}

func fromFargate(ctx context.Context, _ string) (string, error) {
	return fargatePf(ctx)
}

func fargate(ctx context.Context) (string, error) {
	if _, ok := os.LookupEnv("ECS_CONTAINER_METADATA_URI_V4"); !ok {
		return "", fmt.Errorf("not running in fargate")
	}
	launchType, err := ecs.GetLaunchType(ctx)
	if err != nil {
		return "", err
	}
	if launchType == "FARGATE" {
		// If we're running on fargate we strip the hostname
		return "", nil
	}
	return "", fmt.Errorf("not running in fargate")
}

func fromGce(ctx context.Context, _ string) (string, error) {
	return gce.GetCanonicalHostname(ctx)
}

func fromAzure(ctx context.Context, _ string) (string, error) {
	return azure.GetHostname(ctx)
}

func fromFQDN(_ context.Context, _ string) (string, error) {
	//TODO: test this on windows
	fqdn, err := getSystemFQDN()
	if err != nil {
		return "", fmt.Errorf("unable to get FQDN from system: %s", err)
	}
	return fqdn, nil
}

func fromOS(_ context.Context, currentHostname string) (string, error) {
	if currentHostname == "" {
		return os.Hostname()
	}
	return "", fmt.Errorf("skipping OS hostname as a previous provider found a valid hostname")
}

func fromContainer(_ context.Context, _ string) (string, error) {
	// This provider is not implemented as most customers do not provide access to kube-api server, kubelet, or docker socket
	// on their application containers. Providing this access is almost always a not-good idea and could be burdensome for customers.
	return "", fmt.Errorf("container hostname detection not implemented")
}

func fromEC2(ctx context.Context, currentHostname string) (string, error) {
	if ec2.IsDefaultHostname(currentHostname) {
		// If the current hostname is a default one we try to get the instance id
		instanceID, err := ec2.GetInstanceID(ctx)
		if err != nil {
			return "", fmt.Errorf("unable to determine hostname from EC2: %s", err)
		}
		err = validate.ValidHostname(instanceID)
		if err != nil {
			return "", fmt.Errorf("EC2 instance id is not a valid hostname: %s", err)
		}
		return instanceID, nil
	}
	return "", fmt.Errorf("not retrieving hostname from AWS: the host is not an ECS instance and other providers already retrieve non-default hostnames")
}
