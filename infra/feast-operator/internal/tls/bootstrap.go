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
	"slices"
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

	// Validate before the shared helper's TLSVersionOrDie can panic on it.
	minVersion, err := minTLSVersion(profile.MinTLSVersion)
	if err != nil {
		return nil, err
	}

	tlsConfigFn, unsupported := tlspkg.NewTLSConfigFromProfile(profile)
	result.UnsupportedCiphers = unsupported
	if len(unsupported) > 0 {
		logger.Info("TLS profile contains ciphers unsupported by Go", "unsupported", unsupported)
	}

	cipherSuites, err := runtimeUsableCipherSuites(logger, tlsConfigFn, minVersion, profile)
	if err != nil {
		return nil, err
	}

	result.TLSOpts = append(result.TLSOpts, func(c *tls.Config) {
		tlsConfigFn(c)
		// nil means no override: TLS 1.3 (helper leaves CipherSuites unset) or
		// an empty requested list (Go defaults, matching the shared helper).
		if cipherSuites != nil {
			c.CipherSuites = cipherSuites
		}
	})

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

// +kubebuilder:rbac:groups=config.openshift.io,resources=apiservers,verbs=get;list;watch

func fetchTLSProfile(ctx context.Context, k8sClient client.Client) (configv1.TLSProfileSpec, bool, error) {
	fetchCtx, cancel := context.WithTimeout(ctx, FetchTimeout)
	defer cancel()

	profile, err := tlspkg.FetchAPIServerTLSProfile(fetchCtx, k8sClient)
	if err != nil {
		return classifyTLSProfileError(log.FromContext(ctx), err)
	}
	return profile, true, nil
}

func classifyTLSProfileError(logger logr.Logger, err error) (configv1.TLSProfileSpec, bool, error) {
	intermediate := *configv1.TLSProfiles[configv1.TLSProfileIntermediateType]

	switch {
	case apimeta.IsNoMatchError(err), isAPIError(err, apierrors.IsNotFound):
		// API not present — not an OpenShift cluster, fall back to Intermediate.
		logger.Info("TLS profile API not available, using Intermediate defaults", "error", err)
		return intermediate, false, nil
	case isAPIError(err, apierrors.IsForbidden), isAPIError(err, apierrors.IsUnauthorized):
		// The API exists but the operator may not read it; fail startup instead
		// of silently degrading the TLS profile.
		return configv1.TLSProfileSpec{}, false, fmt.Errorf("not authorized to read APIServer TLS profile: %w", err)
	case isTransientError(err):
		logger.Info("Transient API error reading TLS profile, using Intermediate defaults", "error", err)
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

// minTLSVersion validates the profile's minimum TLS version before the shared
// helper's TLSVersionOrDie can panic on it. The configured minimum is honored
// exactly, never floored or raised.
func minTLSVersion(version configv1.TLSProtocolVersion) (uint16, error) {
	switch version {
	case "":
		return tls.VersionTLS12, nil // same default the shared helper applies
	case configv1.VersionTLS10:
		// Go 1.22+ defaults the server minimum to TLS 1.2 only when tls.Config.MinVersion
		// is unset; the explicit minimum set here needs no GODEBUG.
		return tls.VersionTLS10, nil
	case configv1.VersionTLS11:
		return tls.VersionTLS11, nil
	case configv1.VersionTLS12:
		return tls.VersionTLS12, nil
	case configv1.VersionTLS13:
		return tls.VersionTLS13, nil
	default:
		return 0, fmt.Errorf("unsupported minimum TLS version %q", version)
	}
}

// runtimeUsableCipherSuites resolves the profile's cipher names through the
// shared helper and keeps only suites this Go runtime actually negotiates for
// TLS 1.2-and-below handshakes. A non-empty requested list with no usable
// suite is an error: installing nil/empty CipherSuites would silently enable
// Go's default suites.
func runtimeUsableCipherSuites(
	logger logr.Logger, tlsConfigFn func(*tls.Config), minVersion uint16, profile configv1.TLSProfileSpec,
) ([]uint16, error) {
	if minVersion >= tls.VersionTLS13 || len(profile.Ciphers) == 0 {
		return nil, nil
	}

	// Apply the helper's callback to a scratch config to resolve the requested
	// names (IANA and OpenSSL) to cipher IDs.
	scratch := &tls.Config{}
	tlsConfigFn(scratch)

	usable := usableCipherIDs(scratch.CipherSuites)
	if len(usable) == 0 {
		return nil, fmt.Errorf("TLS profile with minimum %s has no cipher suites usable by this Go runtime: %v",
			profile.MinTLSVersion, profile.Ciphers)
	}
	if len(usable) < len(scratch.CipherSuites) {
		var dropped []string
		for _, id := range scratch.CipherSuites {
			if !slices.Contains(usable, id) {
				dropped = append(dropped, tls.CipherSuiteName(id))
			}
		}
		logger.Info("Dropping TLS profile ciphers not usable by the Go runtime", "dropped", dropped)
	}
	return usable, nil
}

// usableCipherIDs filters ids down to suites this Go runtime can negotiate for
// TLS 1.2-and-below handshakes: TLS 1.3-only cipher IDs (0x1300-0x13ff) never
// apply there. Both tls.CipherSuites() and tls.InsecureCipherSuites() are
// runtime-negotiable when explicitly listed (Old's 3DES and RSA-key-exchange
// suites live in the insecure table), so only truly unknown IDs are dropped.
func usableCipherIDs(ids []uint16) []uint16 {
	supported := make(map[uint16]struct{}, 64)
	for _, suite := range tls.CipherSuites() {
		supported[suite.ID] = struct{}{}
	}
	for _, suite := range tls.InsecureCipherSuites() {
		supported[suite.ID] = struct{}{}
	}
	var usable []uint16
	for _, id := range ids {
		if id >= 0x1300 && id <= 0x13ff {
			continue
		}
		if _, ok := supported[id]; ok {
			usable = append(usable, id)
		}
	}
	return usable
}
