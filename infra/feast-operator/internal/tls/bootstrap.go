/*
Copyright 2024 Feast Community.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package tls

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	configv1 "github.com/openshift/api/config/v1"
	tlspkg "github.com/openshift/controller-runtime-common/pkg/tls"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	FetchTimeout = 10 * time.Second
	ALPNH2       = "h2"
	ALPNHTTP11   = "http/1.1"
)

// BootstrapResult holds the TLS configuration resolved at operator startup.
type BootstrapResult struct {
	TLSOpts            []func(*tls.Config)
	ProfileFetched     bool
	ProfileSpec        configv1.TLSProfileSpec
	AdherenceFetched   bool
	AdherencePolicy    configv1.TLSAdherencePolicy
	UnsupportedCiphers []string
}

// Bootstrap fetches the cluster TLS profile and adherence policy and returns
// the resolved TLS options to apply to the metrics server and webhook server.
func Bootstrap(ctx context.Context, k8sClient client.Client) (*BootstrapResult, error) {
	logger := log.FromContext(ctx)
	result := &BootstrapResult{
		TLSOpts: make([]func(*tls.Config), 0, 2),
	}

	profile, profileFetched, err := fetchTLSProfile(ctx, k8sClient)
	if err != nil {
		return nil, err
	}
	result.ProfileFetched = profileFetched
	result.ProfileSpec = profile

	tlsConfigFn, unsupported := tlspkg.NewTLSConfigFromProfile(profile)
	result.UnsupportedCiphers = unsupported
	if len(unsupported) > 0 {
		logger.Info("TLS profile contains ciphers unsupported by Go", "unsupported", unsupported)
	}
	result.TLSOpts = append(result.TLSOpts, tlsConfigFn)

	adherence, adherenceFetched, err := fetchTLSAdherencePolicy(ctx, k8sClient, profileFetched)
	if err != nil {
		return nil, err
	}
	result.AdherenceFetched = adherenceFetched
	result.AdherencePolicy = adherence

	result.TLSOpts = append(result.TLSOpts, func(c *tls.Config) {
		c.NextProtos = []string{ALPNH2, ALPNHTTP11}
	})

	return result, nil
}

func fetchTLSProfile(ctx context.Context, k8sClient client.Client) (configv1.TLSProfileSpec, bool, error) {
	fetchCtx, cancel := context.WithTimeout(ctx, FetchTimeout)
	defer cancel()

	profile, err := tlspkg.FetchAPIServerTLSProfile(fetchCtx, k8sClient)
	if err != nil {
		return classifyTLSProfileError(err)
	}
	return profile, true, nil
}

func classifyTLSProfileError(err error) (configv1.TLSProfileSpec, bool, error) {
	intermediate := *configv1.TLSProfiles[configv1.TLSProfileIntermediateType]

	switch {
	case apimeta.IsNoMatchError(err),
		isAPIError(err, apierrors.IsNotFound),
		isAPIError(err, apierrors.IsForbidden):
		// API not present or no RBAC — not an OpenShift cluster, fall back to Intermediate.
		return intermediate, false, nil
	case isTransientError(err):
		return intermediate, true, nil
	default:
		return configv1.TLSProfileSpec{}, false, fmt.Errorf("unable to read APIServer TLS profile: %w", err)
	}
}

func fetchTLSAdherencePolicy(
	ctx context.Context, k8sClient client.Client, profileFetched bool,
) (configv1.TLSAdherencePolicy, bool, error) {
	if !profileFetched {
		return "", false, nil
	}

	logger := log.FromContext(ctx)
	fetchCtx, cancel := context.WithTimeout(ctx, FetchTimeout)
	defer cancel()

	policy, err := tlspkg.FetchAPIServerTLSAdherencePolicy(fetchCtx, k8sClient)
	if err == nil {
		return policy, true, nil
	}

	ok, classifyErr := classifyTLSAdherenceError(err, logger)
	return "", ok, classifyErr
}

func classifyTLSAdherenceError(err error, logger logr.Logger) (bool, error) {
	switch {
	case apimeta.IsNoMatchError(err), isAPIError(err, apierrors.IsNotFound):
		logger.Info("TLS adherence policy lookup unavailable, watcher will retry", "error", err)
		return true, nil
	case isTransientError(err), isAPIError(err, apierrors.IsInternalError):
		logger.Info("Transient API error reading TLS adherence policy, watcher will retry", "error", err)
		return true, nil
	default:
		return false, fmt.Errorf("unable to read APIServer TLS adherence policy: %w", err)
	}
}

func isTransientError(err error) bool {
	return isAPIError(err, apierrors.IsServiceUnavailable) ||
		isAPIError(err, apierrors.IsTimeout) ||
		isAPIError(err, apierrors.IsServerTimeout) ||
		isAPIError(err, apierrors.IsTooManyRequests) ||
		errors.Is(err, context.DeadlineExceeded)
}

// isAPIError unwraps err and applies check to any *apierrors.StatusError in the chain.
// apierrors.Is* functions do a direct type assertion, missing wrapped errors.
func isAPIError(err error, check func(error) bool) bool {
	var statusErr *apierrors.StatusError
	if errors.As(err, &statusErr) {
		return check(statusErr)
	}
	return check(err)
}
