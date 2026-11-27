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

// Package e2erhoai provides end-to-end (E2E) test coverage for Feast integration with
// Red Hat OpenShift AI (RHOAI) environments.
// This test validates Feast workbench integration with OIDC authentication,
// verifying that OIDC token-based auth works correctly for Feast operations.
package e2erhoai

import (
	"fmt"
	"time"

	. "github.com/feast-dev/feast/infra/feast-operator/test/e2e_rhoai/utils"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Feast OIDC Authentication Integration Testing", Ordered, func() {
	const (
		namespace               = "test-ns-feast-oidc"
		configMapName           = "feast-oidc-wb-cm"
		rolebindingName         = "rb-feast-oidc-test"
		notebookFile            = "test/e2e_rhoai/resources/feast-oidc-auth-test.ipynb"
		pvcFile                 = "test/e2e_rhoai/resources/pvc.yaml"
		permissionFile          = "test/e2e_rhoai/resources/permissions_oidc.py"
		notebookPVC             = "jupyterhub-nb-kube-3aadmin-pvc"
		testDir                 = "/test/e2e_rhoai"
		notebookName            = "feast-oidc-auth-test.ipynb"
		feastDeploymentName     = FeastPrefix + "test-feast-oidc"
		feastCRName             = "test-feast-oidc"
		feastProject            = "driver_ranking_oidc"
		feastOidcYaml           = "test/e2e_rhoai/resources/feast_oidc.yaml"
		feastConfigMapName      = "jupyter-nb-kube-3aadmin-feast-config"
		feastClientConfigMapKey = "driver_ranking_oidc"
	)

	// Verify feast ConfigMap contains OIDC auth type
	verifyFeastOidcConfigMap := func() {
		By(fmt.Sprintf("Listing ConfigMaps and verifying %s exists with OIDC auth", feastConfigMapName))

		expectedContent := []string{
			"project: driver_ranking_oidc",
			"type: oidc",
		}

		const maxRetries = 5
		const retryInterval = 5 * time.Second
		var configMapExists bool
		var err error

		for i := 0; i < maxRetries; i++ {
			exists, listErr := VerifyConfigMapExistsInList(namespace, feastConfigMapName)
			if listErr != nil {
				err = listErr
				if i < maxRetries-1 {
					fmt.Printf("Failed to list ConfigMaps, retrying in %v... (attempt %d/%d)\n", retryInterval, i+1, maxRetries)
					time.Sleep(retryInterval)
					continue
				}
			} else if exists {
				configMapExists = true
				fmt.Printf("ConfigMap %s found in ConfigMap list\n", feastConfigMapName)
				break
			}

			if i < maxRetries-1 {
				fmt.Printf("ConfigMap %s not found in list yet, retrying in %v... (attempt %d/%d)\n", feastConfigMapName, retryInterval, i+1, maxRetries)
				time.Sleep(retryInterval)
			}
		}

		if !configMapExists {
			Expect(err).ToNot(HaveOccurred(), fmt.Sprintf("Failed to find ConfigMap %s in ConfigMap list after %d attempts: %v", feastConfigMapName, maxRetries, err))
		}

		err = VerifyFeastConfigMapContent(namespace, feastConfigMapName, feastClientConfigMapKey, expectedContent)
		Expect(err).ToNot(HaveOccurred(), fmt.Sprintf("Failed to verify Feast ConfigMap %s content: %v", feastConfigMapName, err))
		fmt.Printf("Feast ConfigMap %s verified successfully with OIDC auth type\n", feastConfigMapName)
	}

	BeforeAll(func() {
		By(fmt.Sprintf("Creating test namespace: %s", namespace))
		Expect(CreateNamespace(namespace, testDir)).To(Succeed())
		fmt.Printf("Namespace %s created successfully\n", namespace)
	})

	AfterAll(func() {
		By(fmt.Sprintf("Deleting test namespace: %s", namespace))
		Expect(DeleteNamespace(namespace, testDir)).To(Succeed())
		fmt.Printf("Namespace %s deleted successfully\n", namespace)
	})

	Context("Feast OIDC Authentication Tests", func() {
		BeforeEach(func() {
			By("Applying and validating the OIDC FeatureStore CR")
			ApplyFeastOidcYamlAndVerify(namespace, testDir, feastDeploymentName, feastCRName, feastOidcYaml)

			By("Verify Feature Store CR is in Ready state")
			ValidateFeatureStoreCRStatus(namespace, feastCRName)
		})

		It("Should apply OIDC permissions and verify Feast methods with OIDC auth", func() {
			By("Applying Feast OIDC permissions")
			ApplyFeastOidcPermissions(permissionFile, "/feast-data/driver_ranking_oidc/feature_repo/permissions.py", namespace, feastDeploymentName)

			By("Creating notebook with OIDC token env var")
			CreateOidcNotebookTest(namespace, configMapName, notebookFile, "test/e2e_rhoai/resources/feature_repo",
				pvcFile, rolebindingName, notebookPVC, notebookName, testDir, feastProject)

			By("Verifying Feast ConfigMap was created with OIDC auth type")
			verifyFeastOidcConfigMap()

			By("Monitoring notebook execution")
			MonitorNotebookTest(namespace, notebookName)
		})
	})
})
