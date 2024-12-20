package elephantine

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/urfave/cli/v2"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/clientcredentials"
)

type OpenIDConnectConfig struct {
	Issuer                                                    string            `json:"issuer"`
	AuthorizationEndpoint                                     string            `json:"authorization_endpoint"`
	TokenEndpoint                                             string            `json:"token_endpoint"`
	IntrospectionEndpoint                                     string            `json:"introspection_endpoint"`
	UserinfoEndpoint                                          string            `json:"userinfo_endpoint"`
	EndSessionEndpoint                                        string            `json:"end_session_endpoint"`
	FrontchannelLogoutSessionSupported                        bool              `json:"frontchannel_logout_session_supported"`
	FrontchannelLogoutSupported                               bool              `json:"frontchannel_logout_supported"`
	JwksURI                                                   string            `json:"jwks_uri"`
	CheckSessionIframe                                        string            `json:"check_session_iframe"`
	GrantTypesSupported                                       []string          `json:"grant_types_supported"`
	AcrValuesSupported                                        []string          `json:"acr_values_supported"`
	ResponseTypesSupported                                    []string          `json:"response_types_supported"`
	SubjectTypesSupported                                     []string          `json:"subject_types_supported"`
	IDTokenSigningAlgValuesSupported                          []string          `json:"id_token_signing_alg_values_supported"`
	IDTokenEncryptionAlgValuesSupported                       []string          `json:"id_token_encryption_alg_values_supported"`
	IDTokenEncryptionEncValuesSupported                       []string          `json:"id_token_encryption_enc_values_supported"`
	UserinfoSigningAlgValuesSupported                         []string          `json:"userinfo_signing_alg_values_supported"`
	UserinfoEncryptionAlgValuesSupported                      []string          `json:"userinfo_encryption_alg_values_supported"`
	UserinfoEncryptionEncValuesSupported                      []string          `json:"userinfo_encryption_enc_values_supported"`
	RequestObjectSigningAlgValuesSupported                    []string          `json:"request_object_signing_alg_values_supported"`
	RequestObjectEncryptionAlgValuesSupported                 []string          `json:"request_object_encryption_alg_values_supported"`
	RequestObjectEncryptionEncValuesSupported                 []string          `json:"request_object_encryption_enc_values_supported"`
	ResponseModesSupported                                    []string          `json:"response_modes_supported"`
	RegistrationEndpoint                                      string            `json:"registration_endpoint"`
	TokenEndpointAuthMethodsSupported                         []string          `json:"token_endpoint_auth_methods_supported"`
	TokenEndpointAuthSigningAlgValuesSupported                []string          `json:"token_endpoint_auth_signing_alg_values_supported"`
	IntrospectionEndpointAuthMethodsSupported                 []string          `json:"introspection_endpoint_auth_methods_supported"`
	IntrospectionEndpointAuthSigningAlgValuesSupported        []string          `json:"introspection_endpoint_auth_signing_alg_values_supported"`
	AuthorizationSigningAlgValuesSupported                    []string          `json:"authorization_signing_alg_values_supported"`
	AuthorizationEncryptionAlgValuesSupported                 []string          `json:"authorization_encryption_alg_values_supported"`
	AuthorizationEncryptionEncValuesSupported                 []string          `json:"authorization_encryption_enc_values_supported"`
	ClaimsSupported                                           []string          `json:"claims_supported"`
	ClaimTypesSupported                                       []string          `json:"claim_types_supported"`
	ClaimsParameterSupported                                  bool              `json:"claims_parameter_supported"`
	ScopesSupported                                           []string          `json:"scopes_supported"`
	RequestParameterSupported                                 bool              `json:"request_parameter_supported"`
	RequestURIParameterSupported                              bool              `json:"request_uri_parameter_supported"`
	RequireRequestURIRegistration                             bool              `json:"require_request_uri_registration"`
	CodeChallengeMethodsSupported                             []string          `json:"code_challenge_methods_supported"`
	TLSClientCertificateBoundAccessTokens                     bool              `json:"tls_client_certificate_bound_access_tokens"`
	RevocationEndpoint                                        string            `json:"revocation_endpoint"`
	RevocationEndpointAuthMethodsSupported                    []string          `json:"revocation_endpoint_auth_methods_supported"`
	RevocationEndpointAuthSigningAlgValuesSupported           []string          `json:"revocation_endpoint_auth_signing_alg_values_supported"`
	BackchannelLogoutSupported                                bool              `json:"backchannel_logout_supported"`
	BackchannelLogoutSessionSupported                         bool              `json:"backchannel_logout_session_supported"`
	DeviceAuthorizationEndpoint                               string            `json:"device_authorization_endpoint"`
	BackchannelTokenDeliveryModesSupported                    []string          `json:"backchannel_token_delivery_modes_supported"`
	BackchannelAuthenticationEndpoint                         string            `json:"backchannel_authentication_endpoint"`
	BackchannelAuthenticationRequestSigningAlgValuesSupported []string          `json:"backchannel_authentication_request_signing_alg_values_supported"`
	RequirePushedAuthorizationRequests                        bool              `json:"require_pushed_authorization_requests"`
	PushedAuthorizationRequestEndpoint                        string            `json:"pushed_authorization_request_endpoint"`
	MtlsEndpointAliases                                       map[string]string `json:"mtls_endpoint_aliases"`
	AuthorizationResponseIssParameterSupported                bool              `json:"authorization_response_iss_parameter_supported"`
}

func OpenIDConnectConfigFromURL(
	wellKnown string,
) (*OpenIDConnectConfig, error) {
	var conf OpenIDConnectConfig

	err := UnmarshalHTTPResource(wellKnown, &conf)
	if err != nil {
		return nil, err
	}

	return &conf, nil
}

// OpenIDConnectParameters
//
// Deprecated: Use AuthenticationCLIFlags() instead.
func OpenIDConnectParameters() []cli.Flag {
	return AuthenticationCLIFlags()
}

// AuthenticationCLIFlags returns all the CLI flags that are needed to later
// call AuthenticationConfigFromCLI with the resulting cli.Context.
func AuthenticationCLIFlags() []cli.Flag {
	return []cli.Flag{
		&cli.StringFlag{
			Name:    "oidc-config",
			EnvVars: []string{"OIDC_CONFIG"},
		},
		&cli.StringFlag{
			Name:    "oidc-config-parameter",
			EnvVars: []string{"OIDC_CONFIG_PARAMETER"},
		},
		&cli.StringFlag{
			Name:    "jwt-audience",
			Usage:   "String to validate the aud claim against",
			EnvVars: []string{"JWT_AUDIENCE"},
		},
		&cli.StringFlag{
			Name:    "jwt-scope-prefix",
			Usage:   "Prefix to strip from JWT scopes",
			EnvVars: []string{"JWT_SCOPE_PREFIX"},
		},
		&cli.StringFlag{
			Name:    "client-id",
			EnvVars: []string{"CLIENT_ID"},
		},
		&cli.StringFlag{
			Name:    "client-id-parameter",
			EnvVars: []string{"CLIENT_ID_PARAMETER"},
		},
		&cli.StringFlag{
			Name:    "client-secret",
			EnvVars: []string{"CLIENT_SECRET"},
		},
		&cli.StringFlag{
			Name:    "client-secret-parameter",
			EnvVars: []string{"CLIENT_SECRET_PARAMETER"},
		},
	}
}

type AuthenticationConfig struct {
	OIDCConfig  *OpenIDConnectConfig
	TokenSource oauth2.TokenSource
	AuthParser  *JWTAuthInfoParser

	c           *cli.Context
	paramSource ParameterSource

	m            sync.Mutex
	credErr      error
	clientID     string
	clientSecret string
}

func AuthenticationConfigFromCLI(
	c *cli.Context, paramSource ParameterSource,
	scopes []string,
) (*AuthenticationConfig, error) {
	conf := AuthenticationConfig{
		c:           c,
		paramSource: paramSource,
	}

	oidcConfigURL, err := ResolveParameter(
		c.Context, c, paramSource, "oidc-config")
	if err != nil {
		return nil, fmt.Errorf("resolve OIDC config parameter: %w", err)
	}

	oidcConfig, err := OpenIDConnectConfigFromURL(oidcConfigURL)
	if err != nil {
		return nil, fmt.Errorf("load OIDC config from %q: %w", oidcConfigURL, err)
	}

	conf.OIDCConfig = oidcConfig

	if len(scopes) != 0 {
		ts, err := conf.NewTokenSource(c.Context, scopes)
		if err != nil {
			return nil, fmt.Errorf("create token source: %w", err)
		}

		conf.TokenSource = ts
	}

	audience := c.String("jwt-audience")
	prefix := c.String("jwt-scope-prefix")

	authInfoParser, err := NewJWKSAuthInfoParser(
		c.Context, oidcConfig.JwksURI,
		JWTAuthInfoParserOptions{
			Issuer:      oidcConfig.Issuer,
			Audience:    audience,
			ScopePrefix: prefix,
		})
	if err != nil {
		return nil, fmt.Errorf("retrieve JWKS: %w", err)
	}

	conf.AuthParser = authInfoParser

	return &conf, nil
}

func (conf *AuthenticationConfig) NewTokenSource(
	ctx context.Context, scopes []string,
) (oauth2.TokenSource, error) {
	err := conf.ensureCredentials(ctx)
	if err != nil {
		return nil, err
	}

	clientCredentialsConf := clientcredentials.Config{
		ClientID:     conf.clientID,
		ClientSecret: conf.clientSecret,
		TokenURL:     conf.OIDCConfig.TokenEndpoint,
		Scopes:       scopes,
	}

	return clientCredentialsConf.TokenSource(ctx), nil
}

func (conf *AuthenticationConfig) ensureCredentials(ctx context.Context) error {
	conf.m.Lock()
	defer conf.m.Unlock()

	if conf.credErr != nil {
		return conf.credErr
	}

	err := conf.resolveCredentials(ctx)
	if err != nil {
		conf.credErr = fmt.Errorf("resolve credentials: %w", err)

		return conf.credErr
	}

	return nil
}

func (conf *AuthenticationConfig) resolveCredentials(ctx context.Context) error {
	clientID, err := ResolveParameter(
		ctx, conf.c, conf.paramSource, "client-id",
	)
	if err != nil {
		return fmt.Errorf("resolve client id parameter: %w", err)
	}

	if clientID == "" {
		return errors.New("missing client ID")
	}

	clientSecret, err := ResolveParameter(
		ctx, conf.c, conf.paramSource, "client-secret",
	)
	if err != nil {
		return fmt.Errorf("resolve client secret parameter: %w", err)
	}

	if clientSecret == "" {
		return errors.New("missing client secret")
	}

	conf.clientID = clientID
	conf.clientSecret = clientSecret

	return nil
}
