package transformation

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeTestCert emits a self-signed PEM certificate and returns its path.
func writeTestCert(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "transformation-service-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IsCA:         true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)

	path := filepath.Join(t.TempDir(), "ca.pem")
	require.NoError(t, os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600))
	return path
}

func TestTransformationCredentialsDefaultsToInsecure(t *testing.T) {
	for name, cfg := range map[string]map[string]interface{}{
		"unset":    {},
		"nil":      nil,
		"disabled": {"transformation_service_tls": false},
	} {
		t.Run(name, func(t *testing.T) {
			creds, err := transformationCredentials(cfg)
			assert.NoError(t, err)
			require.NotNil(t, creds)
			assert.Equal(t, "insecure", creds.Info().SecurityProtocol)
		})
	}
}

func TestTransformationCredentialsTLSWithSystemRoots(t *testing.T) {
	creds, err := transformationCredentials(map[string]interface{}{
		"transformation_service_tls": true,
	})
	assert.NoError(t, err)
	require.NotNil(t, creds)
	assert.Equal(t, "tls", creds.Info().SecurityProtocol)
}

func TestTransformationCredentialsTLSWithCustomCA(t *testing.T) {
	creds, err := transformationCredentials(map[string]interface{}{
		"transformation_service_tls":  true,
		"transformation_service_cert": writeTestCert(t),
	})
	assert.NoError(t, err)
	require.NotNil(t, creds)
	assert.Equal(t, "tls", creds.Info().SecurityProtocol)
}

func TestTransformationCredentialsMissingCertFile(t *testing.T) {
	creds, err := transformationCredentials(map[string]interface{}{
		"transformation_service_tls":  true,
		"transformation_service_cert": filepath.Join(t.TempDir(), "absent.pem"),
	})
	assert.Nil(t, creds)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to read transformation_service_cert")
}

func TestTransformationCredentialsUnparsableCertFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "garbage.pem")
	require.NoError(t, os.WriteFile(path, []byte("not a certificate"), 0o600))

	creds, err := transformationCredentials(map[string]interface{}{
		"transformation_service_tls":  true,
		"transformation_service_cert": path,
	})
	assert.Nil(t, creds)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no certificates found")
}
