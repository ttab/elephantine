package elephantine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	vault "github.com/hashicorp/vault/api"
	"github.com/hashicorp/vault/api/auth/kubernetes"
)

const (
	EnvServiceAccountToken         = "SERVICE_ACCOUNT_TOKEN"
	DefaultServiceAccountTokenPath = "/var/run/secrets/kubernetes.io/serviceaccount/token"
	EnvVaultAuthRole               = "VAULT_AUTH_ROLE"
	DefaultAuthRole                = "deploy"
)

// NewVault creates a vault client that can be used as a ParameterSource.
func NewVault() (*Vault, error) {
	config := vault.DefaultConfig()

	client, err := vault.NewClient(config)
	if err != nil {
		return nil, fmt.Errorf("create vault client: %w", err)
	}

	v := Vault{
		parameters: make(map[string]map[string]string),
		Client:     client,
	}

	err = v.authChain()
	if err != nil {
		return nil, err
	}

	return &v, nil
}

// Vault is a helper for setting up a Vault client, also implements
// ParameterSource.
type Vault struct {
	// Cache the data for secrets ...yes that's a silly type declaration.
	parameters map[string]map[string]string

	Client *vault.Client

	stop         chan struct{}
	startOfLease time.Time
	vaultLogin   *vault.Secret
}

// KeepAlive is used to keep the lease on the vault login active, not necessary
// if you're just reading secrets on startup. Returns an error if the lease is
// lost or fails to renew. Returns immediately without an error if a token was
// used to authenticate directly with vault.
func (v *Vault) KeepAlive() error {
	if v.vaultLogin == nil {
		return nil
	}

	for {
		if !v.vaultLogin.Auth.Renewable {
			return errors.New("vault login is not renewable")
		}

		endOfLease := v.startOfLease.Add(
			time.Duration(v.vaultLogin.LeaseDuration) * time.Second)
		leaseDuration := time.Until(endOfLease)

		select {
		case <-v.stop:
			return nil
		case <-time.After(leaseDuration / 3):
		}

		// Renew for the same period as the initial lease.
		secret, err := v.Client.Auth().Token().RenewSelf(
			v.vaultLogin.LeaseDuration,
		)
		if err != nil {
			return fmt.Errorf("renew Vault login lease: %w", err)
		}

		if secret.Auth == nil || secret.Auth.ClientToken == "" {
			return errors.New("no token returned by renewal")
		}

		v.startOfLease = time.Now()
		v.Client.SetToken(secret.Auth.ClientToken)
		v.vaultLogin = secret
	}
}

// Stop the keepalive loop.
func (v *Vault) Stop() {
	close(v.stop)
}

func (v *Vault) authChain() error {
	if v.Client.Token() != "" {
		return nil
	}

	if v.tryTokenFile() {
		return nil
	}

	err := v.kubernetesAuth()
	if err != nil {
		return fmt.Errorf("kubernetes auth failed: %w", err)
	}

	return nil
}

func (v *Vault) kubernetesAuth() error {
	tokenPath := os.Getenv(EnvServiceAccountToken)
	if tokenPath == "" {
		tokenPath = DefaultServiceAccountTokenPath
	}

	role := os.Getenv(EnvVaultAuthRole)
	if role == "" {
		role = DefaultAuthRole
	}

	k8sAuth, err := kubernetes.NewKubernetesAuth(
		role,
		kubernetes.WithServiceAccountTokenPath(tokenPath),
	)
	if err != nil {
		return fmt.Errorf("initialize Kubernetes auth method: %w", err)
	}

	secret, err := v.Client.Auth().Login(context.Background(), k8sAuth)
	if err != nil {
		return fmt.Errorf("log in to vault: %w", err)
	}

	v.startOfLease = time.Now()
	v.vaultLogin = secret

	return nil
}

func (v *Vault) tryTokenFile() bool {
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}

	tokenFile := filepath.Join(home, ".vault-token")

	stat, err := os.Stat(tokenFile)
	if err != nil || stat.IsDir() {
		return false
	}

	tokenData, err := os.ReadFile(tokenFile)
	if err != nil {
		return false
	}

	v.Client.SetToken(string(tokenData))

	return true
}

// GetParameterValue implements ParameterSource.
func (v *Vault) GetParameterValue(ctx context.Context, name string) (string, error) {
	// Use confers syntax of "path:key" to access JSON values.
	path, key, ok := strings.Cut(name, ":")
	if !ok {
		return "", fmt.Errorf("missing ':key' qualifier in name %q", name)
	}

	values, ok := v.parameters[path]
	if !ok {
		d, err := v.dataMapFromEntry(ctx, path)
		if err != nil {
			return "", err
		}

		v.parameters[path] = d

		values = d
	}

	value, ok := values[key]
	if !ok {
		return "", fmt.Errorf("no key %q in %q", key, path)
	}

	return value, nil
}

func (v *Vault) dataMapFromEntry(ctx context.Context, path string) (map[string]string, error) {
	res, err := v.Client.KVv2("secret").Get(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("failed to read from KV store: %w", err)
	}

	d := make(map[string]string)

	for k, v := range res.Data {
		s, ok := v.(string)
		if !ok {
			d[k] = fmt.Sprintf("%v", v)
			continue
		}

		d[k] = s
	}

	return d, nil
}
