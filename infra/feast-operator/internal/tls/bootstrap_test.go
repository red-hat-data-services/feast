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
	"reflect"
	"testing"

	"github.com/go-logr/logr"
	configv1 "github.com/openshift/api/config/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func intermediateProfile() configv1.TLSProfileSpec {
	return *configv1.TLSProfiles[configv1.TLSProfileIntermediateType]
}

func TestClassifyTLSProfileError(t *testing.T) {
	tests := []struct {
		name               string
		err                error
		wantProfileFetched bool
		wantError          bool
		wantIntermediate   bool
	}{
		{
			name:               "NoMatchError returns Intermediate defaults, profileFetched=false",
			err:                &meta.NoResourceMatchError{PartialResource: schema.GroupVersionResource{Group: "config.openshift.io"}},
			wantProfileFetched: false,
			wantError:          false,
			wantIntermediate:   true,
		},
		{
			name:               "NotFound returns Intermediate defaults, profileFetched=false",
			err:                apierrors.NewNotFound(schema.GroupResource{Group: "config.openshift.io", Resource: "apiservers"}, "cluster"),
			wantProfileFetched: false,
			wantError:          false,
			wantIntermediate:   true,
		},
		{
			name:               "ServiceUnavailable is transient, profileFetched=true",
			err:                apierrors.NewServiceUnavailable("api server down"),
			wantProfileFetched: true,
			wantError:          false,
			wantIntermediate:   true,
		},
		{
			name:               "Timeout is transient, profileFetched=true",
			err:                apierrors.NewTimeoutError("timed out", 5),
			wantProfileFetched: true,
			wantError:          false,
			wantIntermediate:   true,
		},
		{
			name:               "ServerTimeout is transient, profileFetched=true",
			err:                apierrors.NewServerTimeout(schema.GroupResource{Group: "config.openshift.io", Resource: "apiservers"}, "GET", 5),
			wantProfileFetched: true,
			wantError:          false,
			wantIntermediate:   true,
		},
		{
			name:               "TooManyRequests is transient, profileFetched=true",
			err:                apierrors.NewTooManyRequests("throttled", 5),
			wantProfileFetched: true,
			wantError:          false,
			wantIntermediate:   true,
		},
		{
			name:               "DeadlineExceeded is transient, profileFetched=true",
			err:                context.DeadlineExceeded,
			wantProfileFetched: true,
			wantError:          false,
			wantIntermediate:   true,
		},
		{
			name:               "Forbidden is fatal, returns error",
			err:                apierrors.NewForbidden(schema.GroupResource{Group: "config.openshift.io", Resource: "apiservers"}, "cluster", errors.New("RBAC")),
			wantProfileFetched: false,
			wantError:          true,
			wantIntermediate:   false,
		},
		{
			name: "Wrapped Forbidden (from FetchAPIServerTLSProfile) is fatal",
			err: fmt.Errorf("failed to get APIServer %q: %w",
				"/cluster",
				apierrors.NewForbidden(schema.GroupResource{Group: "config.openshift.io", Resource: "apiservers"}, "cluster", errors.New("RBAC"))),
			wantProfileFetched: false,
			wantError:          true,
			wantIntermediate:   false,
		},
		{
			name: "Wrapped NotFound falls back to Intermediate",
			err: fmt.Errorf("failed to get APIServer %q: %w",
				"/cluster",
				apierrors.NewNotFound(schema.GroupResource{Group: "config.openshift.io", Resource: "apiservers"}, "cluster")),
			wantProfileFetched: false,
			wantError:          false,
			wantIntermediate:   true,
		},
		{
			name:               "Unauthorized is fatal, returns error",
			err:                apierrors.NewUnauthorized("no token"),
			wantProfileFetched: false,
			wantError:          true,
			wantIntermediate:   false,
		},
		{
			name:               "InternalServerError is fatal, returns error",
			err:                apierrors.NewInternalError(errors.New("crash")),
			wantProfileFetched: false,
			wantError:          true,
			wantIntermediate:   false,
		},
		{
			name:               "Generic error is fatal, returns error",
			err:                errors.New("something unexpected"),
			wantProfileFetched: false,
			wantError:          true,
			wantIntermediate:   false,
		},
		{
			name:               "Wrapped DeadlineExceeded is transient",
			err:                errors.Join(errors.New("fetch failed"), context.DeadlineExceeded),
			wantProfileFetched: true,
			wantError:          false,
			wantIntermediate:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			profile, fetched, err := classifyTLSProfileError(logr.Discard(), tt.err)

			if tt.wantError && err == nil {
				t.Errorf("expected error, got nil")
			}
			if !tt.wantError && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			if fetched != tt.wantProfileFetched {
				t.Errorf("profileFetched = %v, want %v", fetched, tt.wantProfileFetched)
			}
			if tt.wantIntermediate {
				intermediate := intermediateProfile()
				if profile.MinTLSVersion != intermediate.MinTLSVersion {
					t.Errorf("MinTLSVersion = %v, want %v (Intermediate)", profile.MinTLSVersion, intermediate.MinTLSVersion)
				}
			}
		})
	}
}

func TestIsTransientError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"ServiceUnavailable", apierrors.NewServiceUnavailable("down"), true},
		{"Timeout", apierrors.NewTimeoutError("slow", 5), true},
		{"ServerTimeout", apierrors.NewServerTimeout(schema.GroupResource{}, "GET", 5), true},
		{"TooManyRequests", apierrors.NewTooManyRequests("throttled", 5), true},
		{"DeadlineExceeded", context.DeadlineExceeded, true},
		{"Wrapped DeadlineExceeded", errors.Join(errors.New("wrapper"), context.DeadlineExceeded), true},
		{"NotFound", apierrors.NewNotFound(schema.GroupResource{}, "x"), false},
		{"Forbidden", apierrors.NewForbidden(schema.GroupResource{}, "x", errors.New("RBAC")), false},
		{"Unauthorized", apierrors.NewUnauthorized("no token"), false},
		{"InternalError", apierrors.NewInternalError(errors.New("crash")), false},
		{"Generic error", errors.New("oops"), false},
		{"Nil", nil, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isTransientError(tt.err); got != tt.want {
				t.Errorf("isTransientError() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestIntermediateProfileHasExpectedDefaults(t *testing.T) {
	profile := intermediateProfile()

	if profile.MinTLSVersion != configv1.VersionTLS12 {
		t.Errorf("Intermediate MinTLSVersion = %v, want %v", profile.MinTLSVersion, configv1.VersionTLS12)
	}
	if len(profile.Ciphers) == 0 {
		t.Error("Intermediate profile should have non-empty cipher list")
	}
}

func TestMinTLSVersion(t *testing.T) {
	tests := []struct {
		name    string
		version configv1.TLSProtocolVersion
		want    uint16
		wantErr bool
	}{
		{"empty defaults to TLS 1.2 like the shared helper", "", tls.VersionTLS12, false},
		{"VersionTLS10 is honored exactly", configv1.VersionTLS10, tls.VersionTLS10, false},
		{"VersionTLS11 is honored exactly", configv1.VersionTLS11, tls.VersionTLS11, false},
		{"VersionTLS12", configv1.VersionTLS12, tls.VersionTLS12, false},
		{"VersionTLS13", configv1.VersionTLS13, tls.VersionTLS13, false},
		{"unknown version errors instead of panicking", "VersionTLS14", 0, true},
		{"garbage version errors instead of panicking", "banana", 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := minTLSVersion(tt.version)

			if tt.wantErr && err == nil {
				t.Errorf("expected error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("minTLSVersion() = %v, want %v", got, tt.want)
			}
		})
	}
}

func tlsTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := configv1.AddToScheme(scheme); err != nil {
		t.Fatalf("failed to add configv1 to scheme: %v", err)
	}
	return scheme
}

// fakeBootstrapClient returns a client whose Get fails with getErr when set,
// otherwise serving objs like a real API server would.
func fakeBootstrapClient(t *testing.T, getErr error, objs ...client.Object) client.Client {
	t.Helper()
	builder := fake.NewClientBuilder().WithScheme(tlsTestScheme(t)).WithObjects(objs...)
	if getErr != nil {
		builder = builder.WithInterceptorFuncs(interceptor.Funcs{
			Get: func(_ context.Context, _ client.WithWatch, _ client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
				return getErr
			},
		})
	}
	return builder.Build()
}

func apiserverWithCustomProfile(spec configv1.TLSProfileSpec) *configv1.APIServer {
	return &configv1.APIServer{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
		Spec: configv1.APIServerSpec{
			TLSSecurityProfile: &configv1.TLSSecurityProfile{
				Type:   configv1.TLSProfileCustomType,
				Custom: &configv1.CustomTLSProfile{TLSProfileSpec: spec},
			},
		},
	}
}

func applyTLSOpts(result *BootstrapResult) *tls.Config {
	cfg := &tls.Config{}
	for _, opt := range result.TLSOpts {
		opt(cfg)
	}
	return cfg
}

func TestBootstrap_ReaderErrors(t *testing.T) {
	// The Intermediate fallback keeps its six runtime-usable ECDHE suites:
	// the TLS 1.3-only names in the profile never apply to TLS 1.2 handshakes.
	intermediateCiphers := []uint16{
		tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
		tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
		tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
		tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
		tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256,
		tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305_SHA256,
	}

	tests := []struct {
		name               string
		err                error
		wantError          bool
		wantProfileFetched bool
	}{
		{
			name:               "NoMatchError falls back to Intermediate without watcher",
			err:                &meta.NoResourceMatchError{PartialResource: schema.GroupVersionResource{Group: "config.openshift.io"}},
			wantProfileFetched: false,
		},
		{
			name:               "NotFound falls back to Intermediate without watcher",
			err:                apierrors.NewNotFound(schema.GroupResource{Group: "config.openshift.io", Resource: "apiservers"}, "cluster"),
			wantProfileFetched: false,
		},
		{
			name:               "ServiceUnavailable is transient, watcher stays enabled",
			err:                apierrors.NewServiceUnavailable("api server down"),
			wantProfileFetched: true,
		},
		{
			name:      "Forbidden fails startup",
			err:       apierrors.NewForbidden(schema.GroupResource{Group: "config.openshift.io", Resource: "apiservers"}, "cluster", errors.New("RBAC")),
			wantError: true,
		},
		{
			name: "Wrapped Forbidden fails startup",
			err: fmt.Errorf("failed to get APIServer %q: %w",
				"/cluster",
				apierrors.NewForbidden(schema.GroupResource{Group: "config.openshift.io", Resource: "apiservers"}, "cluster", errors.New("RBAC"))),
			wantError: true,
		},
		{
			name:      "Unauthorized fails startup",
			err:       apierrors.NewUnauthorized("no token"),
			wantError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := Bootstrap(context.Background(), fakeBootstrapClient(t, tt.err))

			if tt.wantError && err == nil {
				t.Fatal("expected error, got nil")
			}
			if !tt.wantError && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tt.wantError {
				if result != nil {
					t.Error("expected nil BootstrapResult on error")
				}
				return
			}

			if result.ProfileFetched != tt.wantProfileFetched {
				t.Errorf("ProfileFetched = %v, want %v", result.ProfileFetched, tt.wantProfileFetched)
			}

			cfg := applyTLSOpts(result)
			if cfg.MinVersion != tls.VersionTLS12 {
				t.Errorf("MinVersion = %v, want %v (Intermediate)", cfg.MinVersion, tls.VersionTLS12)
			}
			if !reflect.DeepEqual(cfg.CipherSuites, intermediateCiphers) {
				t.Errorf("CipherSuites = %v, want %v (Intermediate usable subset)", cfg.CipherSuites, intermediateCiphers)
			}
			if len(cfg.NextProtos) != 2 || cfg.NextProtos[0] != ALPNH2 || cfg.NextProtos[1] != ALPNHTTP11 {
				t.Errorf("NextProtos = %v, want [%s, %s]", cfg.NextProtos, ALPNH2, ALPNHTTP11)
			}
		})
	}
}

func TestBootstrap_CustomProfile(t *testing.T) {
	tests := []struct {
		name             string
		profile          configv1.TLSProfileSpec
		wantError        bool
		wantMinVersion   uint16
		wantCipherSuites []uint16
	}{
		{
			name: "TLS 1.0 minimum is honored exactly, never floored",
			profile: configv1.TLSProfileSpec{
				MinTLSVersion: configv1.VersionTLS10,
				Ciphers:       []string{"ECDHE-RSA-AES128-GCM-SHA256"},
			},
			wantMinVersion:   tls.VersionTLS10,
			wantCipherSuites: []uint16{tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256},
		},
		{
			name: "TLS 1.3 minimum never sets CipherSuites",
			profile: configv1.TLSProfileSpec{
				MinTLSVersion: configv1.VersionTLS13,
				Ciphers:       []string{"TLS_AES_128_GCM_SHA256", "TLS_CHACHA20_POLY1305_SHA256"},
			},
			wantMinVersion: tls.VersionTLS13,
		},
		{
			name: "empty cipher list keeps Go defaults",
			profile: configv1.TLSProfileSpec{
				MinTLSVersion: configv1.VersionTLS12,
			},
			wantMinVersion: tls.VersionTLS12,
		},
		{
			name: "mixed list keeps only the usable subset",
			profile: configv1.TLSProfileSpec{
				MinTLSVersion: configv1.VersionTLS12,
				Ciphers:       []string{"TLS_AES_128_GCM_SHA256", "ECDHE-RSA-AES128-GCM-SHA256", "NOT-A-CIPHER"},
			},
			wantMinVersion:   tls.VersionTLS12,
			wantCipherSuites: []uint16{tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256},
		},
		{
			name: "all ciphers TLS 1.3-only with a TLS 1.2 minimum errors",
			profile: configv1.TLSProfileSpec{
				MinTLSVersion: configv1.VersionTLS12,
				Ciphers:       []string{"TLS_AES_128_GCM_SHA256", "TLS_CHACHA20_POLY1305_SHA256"},
			},
			wantError: true,
		},
		{
			name: "all ciphers unresolvable errors instead of installing Go defaults",
			profile: configv1.TLSProfileSpec{
				MinTLSVersion: configv1.VersionTLS12,
				Ciphers:       []string{"NOT-A-CIPHER"},
			},
			wantError: true,
		},
		{
			name: "RSA key exchange suite is retained (runtime-negotiable via InsecureCipherSuites)",
			profile: configv1.TLSProfileSpec{
				MinTLSVersion: configv1.VersionTLS12,
				// Resolves to TLS_RSA_WITH_AES_128_GCM_SHA256: negotiable when
				// explicitly listed, so honoring keeps it.
				Ciphers: []string{"AES128-GCM-SHA256"},
			},
			wantMinVersion:   tls.VersionTLS12,
			wantCipherSuites: []uint16{tls.TLS_RSA_WITH_AES_128_GCM_SHA256},
		},
		{
			name: "invalid minimum version errors at startup instead of panicking",
			profile: configv1.TLSProfileSpec{
				MinTLSVersion: "VersionTLS14",
				Ciphers:       []string{"ECDHE-RSA-AES128-GCM-SHA256"},
			},
			wantError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			k8sClient := fakeBootstrapClient(t, nil, apiserverWithCustomProfile(tt.profile))
			result, err := Bootstrap(context.Background(), k8sClient)

			if tt.wantError {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				if result != nil {
					t.Error("expected nil BootstrapResult on error")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			cfg := applyTLSOpts(result)
			if cfg.MinVersion != tt.wantMinVersion {
				t.Errorf("MinVersion = %v, want %v", cfg.MinVersion, tt.wantMinVersion)
			}
			if !reflect.DeepEqual(cfg.CipherSuites, tt.wantCipherSuites) {
				t.Errorf("CipherSuites = %v, want %v", cfg.CipherSuites, tt.wantCipherSuites)
			}
		})
	}
}

func TestClassifyTLSProfileError_AllTransientErrorsSetProfileFetched(t *testing.T) {
	transientErrors := []error{
		apierrors.NewServiceUnavailable("down"),
		apierrors.NewTimeoutError("slow", 5),
		apierrors.NewServerTimeout(schema.GroupResource{Group: "config.openshift.io", Resource: "apiservers"}, "GET", 5),
		apierrors.NewTooManyRequests("throttled", 5),
		context.DeadlineExceeded,
	}

	for _, err := range transientErrors {
		_, fetched, classifyErr := classifyTLSProfileError(logr.Discard(), err)
		if classifyErr != nil {
			t.Errorf("transient error %T should not return error, got: %v", err, classifyErr)
		}
		if !fetched {
			t.Errorf("transient error %T should set profileFetched=true", err)
		}
	}
}

func TestClassifyTLSProfileError_NonTransientErrorsDoNotSetProfileFetched(t *testing.T) {
	nonTransientErrors := []error{
		&meta.NoResourceMatchError{PartialResource: schema.GroupVersionResource{Group: "config.openshift.io"}},
		apierrors.NewNotFound(schema.GroupResource{}, "cluster"),
	}

	for _, err := range nonTransientErrors {
		_, fetched, classifyErr := classifyTLSProfileError(logr.Discard(), err)
		if classifyErr != nil {
			t.Errorf("graceful error %T should not return error, got: %v", err, classifyErr)
		}
		if fetched {
			t.Errorf("graceful error %T should set profileFetched=false", err)
		}
	}
}

func TestClassifyTLSProfileError_FatalErrorsReturnError(t *testing.T) {
	fatalErrors := []error{
		apierrors.NewUnauthorized("no token"),
		apierrors.NewInternalError(errors.New("crash")),
		errors.New("unexpected"),
	}

	for _, err := range fatalErrors {
		_, _, classifyErr := classifyTLSProfileError(logr.Discard(), err)
		if classifyErr == nil {
			t.Errorf("fatal error %T should return error", err)
		}
	}
}

func TestClassifyTLSProfileError_IntermediateProfileAlwaysApplied(t *testing.T) {
	allNonFatalErrors := []error{
		&meta.NoResourceMatchError{PartialResource: schema.GroupVersionResource{Group: "config.openshift.io"}},
		apierrors.NewNotFound(schema.GroupResource{}, "cluster"),
		apierrors.NewServiceUnavailable("down"),
		apierrors.NewTimeoutError("slow", 5),
		apierrors.NewServerTimeout(schema.GroupResource{}, "GET", 5),
		apierrors.NewTooManyRequests("throttled", 5),
		context.DeadlineExceeded,
	}

	intermediate := intermediateProfile()
	for _, err := range allNonFatalErrors {
		profile, _, classifyErr := classifyTLSProfileError(logr.Discard(), err)
		if classifyErr != nil {
			t.Fatalf("unexpected error for %T: %v", err, classifyErr)
		}
		if profile.MinTLSVersion != intermediate.MinTLSVersion {
			t.Errorf("for error %T: MinTLSVersion = %v, want Intermediate (%v)", err, profile.MinTLSVersion, intermediate.MinTLSVersion)
		}
		if len(profile.Ciphers) != len(intermediate.Ciphers) {
			t.Errorf("for error %T: got %d ciphers, want %d (Intermediate)", err, len(profile.Ciphers), len(intermediate.Ciphers))
		}
	}
}

func TestBootstrapResult_NextProtosAlwaysSet(t *testing.T) {
	// Verify the ALPN opt wiring in isolation; TestBootstrap_ReaderErrors checks
	// the same behavior end-to-end on a real BootstrapResult.
	result := &BootstrapResult{
		TLSOpts: make([]func(*tls.Config), 0, 2),
	}
	result.TLSOpts = append(result.TLSOpts, func(c *tls.Config) {
		c.NextProtos = []string{ALPNH2, ALPNHTTP11}
	})

	cfg := &tls.Config{}
	for _, opt := range result.TLSOpts {
		opt(cfg)
	}

	if len(cfg.NextProtos) != 2 || cfg.NextProtos[0] != ALPNH2 || cfg.NextProtos[1] != ALPNHTTP11 {
		t.Errorf("NextProtos = %v, want [%s, %s]", cfg.NextProtos, ALPNH2, ALPNHTTP11)
	}
}

func TestClassifyTLSAdherenceError(t *testing.T) {
	logger := logr.Discard()

	tests := []struct {
		name                 string
		err                  error
		wantAdherenceFetched bool
		wantError            bool
	}{
		{
			name:                 "NotFound sets adherenceFetched for watcher retry",
			err:                  apierrors.NewNotFound(schema.GroupResource{Group: "config.openshift.io", Resource: "apiservers"}, "cluster"),
			wantAdherenceFetched: true,
		},
		{
			name:                 "NoMatch sets adherenceFetched for watcher retry",
			err:                  &meta.NoResourceMatchError{PartialResource: schema.GroupVersionResource{Group: "config.openshift.io"}},
			wantAdherenceFetched: true,
		},
		{
			name:                 "ServiceUnavailable is transient, adherenceFetched=true",
			err:                  apierrors.NewServiceUnavailable("api server down"),
			wantAdherenceFetched: true,
		},
		{
			name:                 "InternalError is transient, adherenceFetched=true",
			err:                  apierrors.NewInternalError(errors.New("crash")),
			wantAdherenceFetched: true,
		},
		{
			name:      "Forbidden is fatal",
			err:       apierrors.NewForbidden(schema.GroupResource{}, "cluster", errors.New("RBAC")),
			wantError: true,
		},
		{
			name: "Wrapped NotFound sets adherenceFetched for watcher retry",
			err: fmt.Errorf("failed to get APIServer %q: %w",
				"/cluster",
				apierrors.NewNotFound(schema.GroupResource{Group: "config.openshift.io", Resource: "apiservers"}, "cluster")),
			wantAdherenceFetched: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fetched, err := classifyTLSAdherenceError(tt.err, logger)
			if tt.wantError && err == nil {
				t.Fatal("expected error, got nil")
			}
			if !tt.wantError && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if fetched != tt.wantAdherenceFetched {
				t.Fatalf("adherenceFetched = %v, want %v", fetched, tt.wantAdherenceFetched)
			}
		})
	}
}

func TestFetchTLSAdherencePolicy_SkipsWhenProfileNotFetched(t *testing.T) {
	policy, fetched, err := fetchTLSAdherencePolicy(context.Background(), nil, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fetched {
		t.Fatal("expected adherenceFetched=false when profile was not fetched")
	}
	if policy != "" {
		t.Fatalf("expected empty policy, got %q", policy)
	}
}
