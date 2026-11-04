/*
Copyright 2026 Feast Community.

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

package services

import (
	"os"
	"sync"
	"time"

	utilcluster "github.com/opendatahub-io/odh-platform-utilities/pkg/cluster"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// platformTypeEnv is the explicit install-type override honored by
// odh-platform-utilities DetectPlatform. Recognized values are
// OpenDataHub, ManagedRHOAI, SelfManagedRHOAI, and XKS.
const platformTypeEnv = "ODH_PLATFORM_TYPE"

// platformDetectRetry is how long to wait before probing the cluster again
// after a failed install-type detection. A forbidden or transient API error
// must not be retried on every FeatureStore reconcile.
const platformDetectRetry = 30 * time.Second

// installPlatformKind is the install family that selects dashboard peers.
// ODH and RHOAI dashboards do not share pod labels.
type installPlatformKind int

const (
	// installPlatformUnknown means detection has not succeeded. The dashboard
	// policy keeps every known peer so a detection failure cannot drop the
	// dashboard. The next successful detection replaces that policy.
	installPlatformUnknown installPlatformKind = iota
	installPlatformODH
	installPlatformRHOAI
)

var (
	platformMu          sync.Mutex
	installPlatform     = installPlatformUnknown
	platformResolved    bool
	platformReader      client.Reader
	platformNextAttempt time.Time
	platformLoggedErr   bool
)

// SetInstallPlatformReader supplies the API reader used to detect whether
// this process is running on ODH or RHOAI. The reader must bypass the
// controller cache: the OLM objects DetectPlatform reads are not watched.
// Passing nil disables detection.
func SetInstallPlatformReader(reader client.Reader) {
	platformMu.Lock()
	defer platformMu.Unlock()
	platformReader = reader
	platformResolved = false
	platformNextAttempt = time.Time{}
	platformLoggedErr = false
}

func currentInstallPlatform() installPlatformKind {
	platformMu.Lock()
	defer platformMu.Unlock()
	return installPlatform
}

// ensureInstallPlatform resolves the install type once per process.
// DetectPlatform is the platform utility: an explicit ODH_PLATFORM_TYPE
// wins, otherwise it probes the managed RHOAI catalog and the
// rhods-operator install. A failure leaves the type unknown and is retried.
func (feast *FeastServices) ensureInstallPlatform() {
	platformMu.Lock()
	if platformResolved || platformReader == nil || time.Now().Before(platformNextAttempt) {
		platformMu.Unlock()
		return
	}
	reader := platformReader
	platformMu.Unlock()

	ctx := feast.Handler.Context
	detected, err := utilcluster.DetectPlatform(ctx, reader, os.Getenv(platformTypeEnv), "")

	platformMu.Lock()
	defer platformMu.Unlock()
	if platformResolved {
		return
	}
	logger := log.FromContext(ctx)
	if err != nil {
		platformNextAttempt = time.Now().Add(platformDetectRetry)
		if !platformLoggedErr {
			platformLoggedErr = true
			logger.Error(err, "failed to detect ODH/RHOAI install type; dashboard NetworkPolicy allows both until detection succeeds")
		}
		return
	}

	kind := classifyInstallPlatform(detected)
	if kind == installPlatformUnknown {
		platformNextAttempt = time.Now().Add(platformDetectRetry)
		if !platformLoggedErr {
			platformLoggedErr = true
			logger.Info("unrecognized install platform; dashboard NetworkPolicy allows both ODH and RHOAI peers", "platform", string(detected))
		}
		return
	}

	installPlatform = kind
	platformResolved = true
	platformNextAttempt = time.Time{}
	logger.Info("Detected install platform for dashboard NetworkPolicy", "platform", string(detected))
}

func classifyInstallPlatform(platform utilcluster.Platform) installPlatformKind {
	switch platform {
	case utilcluster.ManagedRhoai, utilcluster.SelfManagedRhoai:
		return installPlatformRHOAI
	case utilcluster.OpenDataHub, utilcluster.XKS:
		return installPlatformODH
	default:
		return installPlatformUnknown
	}
}

func setInstallPlatformForTest(kind installPlatformKind) {
	platformMu.Lock()
	defer platformMu.Unlock()
	installPlatform = kind
	platformResolved = true
	platformNextAttempt = time.Time{}
}

func resetInstallPlatformForTest() {
	platformMu.Lock()
	defer platformMu.Unlock()
	installPlatform = installPlatformUnknown
	platformResolved = false
	platformReader = nil
	platformNextAttempt = time.Time{}
	platformLoggedErr = false
}
