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

package utils

import (
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"strings"

	testutils "github.com/feast-dev/feast/infra/feast-operator/test/utils"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// ApplyFeastOidcPermissions writes the OIDC permissions file into the Feast registry pod
// using kubectl exec (avoids `oc cp` which requires tar in the container),
// then runs `feast apply` to register the permissions.
func ApplyFeastOidcPermissions(fileName string, registryFilePath string, namespace string, podNamePrefix string) {
	By("Applying Feast OIDC permissions to the Feast registry pod")

	By(fmt.Sprintf("Finding pod with prefix %q in namespace %q", podNamePrefix, namespace))
	pod, err := getPodByPrefix(namespace, podNamePrefix)
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	ExpectWithOffset(1, pod).NotTo(BeNil())

	podName := pod.Name
	fmt.Printf("Found pod: %s\n", podName)

	// Read the permissions file content locally
	content, err := os.ReadFile(fileName)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), fmt.Sprintf("Failed to read permissions file %s", fileName))

	idx := strings.LastIndex(registryFilePath, "/")
	ExpectWithOffset(1, idx).To(BeNumerically(">", 0), "registryFilePath must include a directory component")
	dir := registryFilePath[:idx]

	By(fmt.Sprintf("Writing permissions file to %s in pod %s", registryFilePath, podName))
	cmd := exec.Command(
		"kubectl", "exec", podName,
		"-n", namespace,
		"-c", "registry",
		"--",
		"mkdir", "-p", dir,
	)
	_, err = testutils.Run(cmd, "/test/e2e_rhoai")
	ExpectWithOffset(1, err).NotTo(HaveOccurred())

	encoded := base64.StdEncoding.EncodeToString(content)
	shellCmd := fmt.Sprintf("echo '%s' | base64 -d > %s", encoded, registryFilePath)
	cmd = exec.Command(
		"kubectl", "exec", podName,
		"-n", namespace,
		"-c", "registry",
		"--",
		"sh", "-c", shellCmd,
	)
	_, err = testutils.Run(cmd, "/test/e2e_rhoai")
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	fmt.Printf("Successfully wrote OIDC permissions file to pod: %s\n", podName)

	By("Running feast apply inside the Feast registry pod")
	cmd = exec.Command(
		"kubectl", "exec", podName,
		"-n", namespace,
		"-c", "registry",
		"--",
		"sh", "-c",
		"cd /feast-data/driver_ranking_oidc/feature_repo && feast apply",
	)
	_, err = testutils.Run(cmd, "/test/e2e_rhoai")
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	fmt.Println("Feast OIDC permissions apply executed successfully")

	By("Validating that Feast OIDC permission has been applied")
	cmd = exec.Command(
		"kubectl", "exec", podName,
		"-n", namespace,
		"-c", "registry",
		"--",
		"feast", "permissions", "list",
	)

	output, err := testutils.Run(cmd, "/test/e2e_rhoai")
	ExpectWithOffset(1, err).NotTo(HaveOccurred())

	ExpectWithOffset(1, output).To(ContainSubstring("admin_group_permission"), "Expected permission 'admin_group_permission' to exist")
	fmt.Println("Verified: Feast OIDC permission 'admin_group_permission' exists")
}

// ApplyFeastOidcYamlAndVerify applies the OIDC FeatureStore manifest and waits for the
// deployment to become available. Unlike ApplyFeastYamlAndVerify, this skips postgres
// table checks and git repo init container verification since the OIDC CR uses default stores.
func ApplyFeastOidcYamlAndVerify(namespace string, testDir string, feastDeploymentName string, feastCRName string, feastYAMLFilePath string) {
	By("Applying OIDC Feast yaml for Feature store CR")
	cmd := exec.Command("kubectl", "apply", "-n", namespace,
		"-f", feastYAMLFilePath)
	_, err := testutils.Run(cmd, testDir)
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	CheckDeployment(namespace, feastDeploymentName)
}

// CreateOidcNotebookTest performs all the setup steps and creates a notebook with OIDC token injected.
func CreateOidcNotebookTest(namespace, configMapName, notebookFile, featureRepoPath, pvcFile, rolebindingName, notebookPVC, notebookName, testDir string, feastProject string) {
	prepareNotebookTestResources(namespace, configMapName, notebookFile, featureRepoPath, pvcFile, rolebindingName, notebookPVC, testDir)

	nbParams := GetOidcNotebookParams(namespace, configMapName, notebookPVC, notebookName, testDir, feastProject)
	By("Creating Jupyter Notebook with OIDC token")
	Expect(CreateNotebook(nbParams)).To(Succeed(), "Failed to create OIDC notebook")
}

// BuildOidcNotebookCommand builds the command for executing the OIDC notebook.
func BuildOidcNotebookCommand(notebookName string) []string {
	return []string{
		"/bin/sh",
		"-c",
		fmt.Sprintf(
			"pip install papermill && "+
				"mkdir -p /opt/app-root/src/feature_repo && "+
				"cp -rL /opt/app-root/notebooks/* /opt/app-root/src/feature_repo/ && "+
				"(papermill /opt/app-root/notebooks/%s /opt/app-root/src/output.ipynb --kernel python3 && "+
				"echo '✅ Notebook executed successfully' || "+
				"(echo '❌ Notebook execution failed' && "+
				"cp /opt/app-root/src/output.ipynb /opt/app-root/src/failed_output.ipynb && "+
				"echo '📄 Copied failed notebook to failed_output.ipynb')) && "+
				"jupyter nbconvert --to notebook --stdout /opt/app-root/src/output.ipynb || echo '⚠️ nbconvert failed' && "+
				"sleep 100; exit 0",
			notebookName,
		),
	}
}

// GetOidcNotebookParams builds NotebookTemplateParams with FEAST_OIDC_TOKEN set from TOKEN env var.
func GetOidcNotebookParams(namespace, configMapName, notebookPVC, notebookName, testDir string, feastProject string) NotebookTemplateParams {
	username := GetOCUser(testDir)
	command := BuildOidcNotebookCommand(notebookName)
	oidcToken := strings.TrimSpace(os.Getenv("TOKEN"))
	ExpectWithOffset(1, oidcToken).NotTo(BeEmpty(), "TOKEN env var must be set for OIDC notebook test")

	getEnv := func(key string) string {
		val, _ := os.LookupEnv(key)
		return val
	}

	return NotebookTemplateParams{
		Namespace:             namespace,
		IngressDomain:         GetIngressDomain(testDir),
		OpenDataHubNamespace:  getEnv("APPLICATIONS_NAMESPACE"),
		NotebookImage:         getEnv("NOTEBOOK_IMAGE"),
		NotebookConfigMapName: configMapName,
		NotebookPVC:           notebookPVC,
		Username:              username,
		OC_TOKEN:              GetOCToken(testDir),
		OC_SERVER:             GetOCServer(testDir),
		NotebookFile:          notebookName,
		Command:               "[\"" + strings.Join(command, "\",\"") + "\"]",
		PipIndexUrl:           getEnv("PIP_INDEX_URL"),
		PipTrustedHost:        getEnv("PIP_TRUSTED_HOST"),
		FeastVersion:          getEnv("FEAST_VERSION"),
		OpenAIAPIKey:          getEnv("OPENAI_API_KEY"),
		FeastProject:          feastProject,
		AdditionalEnv: map[string]string{
			"FEAST_OIDC_TOKEN": oidcToken,
		},
	}
}
