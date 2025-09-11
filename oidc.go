package elephantine

import (
	"context"
	"errors"
	"fmt"

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

// AuthenticationCLIFlags returns all the CLI flags that are needed to later
// call AuthenticationConfigFromCLI with the resulting cli.Context.
func AuthenticationCLIFlags() []cli.Flag {
	return []cli.Flag{
		&cli.StringFlag{
			Name:    "oidc-config",
			EnvVars: []string{"OIDC_CONFIG"},
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
			Name:    "client-secret",
			EnvVars: []string{"CLIENT_SECRET"},
		},
	}
}

type AuthenticationConfig struct {
	OIDCConfig  *OpenIDConnectConfig
	TokenSource oauth2.TokenSource
	AuthParser  *JWTAuthInfoParser

	s AuthenticationSettings
}

type AuthenticationSettings struct {
	OIDCConfig   string
	Audience     string
	ScopePrefix  string
	ClientID     string
	ClientSecret string
}

func AuthenticationConfigFromCLI(
	c *cli.Context, scopes []string,
) (*AuthenticationConfig, error) {
	return AuthenticationConfigFromSettings(
		c.Context,
		AuthenticationSettings{
			OIDCConfig:   c.String("oidc-config"),
			ClientID:     c.String("client-id"),
			ClientSecret: c.String("client-secret"),
			Audience:     c.String("jwt-audience"),
			ScopePrefix:  c.String("jwt-scope-prefix"),
		},
		scopes)
}

func AuthenticationConfigFromSettings(
	ctx context.Context, settings AuthenticationSettings, scopes []string,
) (*AuthenticationConfig, error) {
	conf := AuthenticationConfig{
		s: settings,
	}

	oidcConfig, err := OpenIDConnectConfigFromURL(settings.OIDCConfig)
	if err != nil {
		return nil, fmt.Errorf("load OIDC config from %q: %w", settings.OIDCConfig, err)
	}

	conf.OIDCConfig = oidcConfig

	if len(scopes) != 0 {
		ts, err := conf.NewTokenSource(ctx, scopes)
		if err != nil {
			return nil, fmt.Errorf("create token source: %w", err)
		}

		conf.TokenSource = ts
	}

	authInfoParser, err := NewJWKSAuthInfoParser(
		ctx, oidcConfig.JwksURI,
		JWTAuthInfoParserOptions{
			Issuer:      oidcConfig.Issuer,
			Audience:    settings.Audience,
			ScopePrefix: settings.ScopePrefix,
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
	if conf.s.ClientID == "" {
		return nil, errors.New("missing client ID")
	}

	if conf.s.ClientSecret == "" {
		return nil, errors.New("missing client secret")
	}

	clientCredentialsConf := clientcredentials.Config{
		ClientID:     conf.s.ClientID,
		ClientSecret: conf.s.ClientSecret,
		TokenURL:     conf.OIDCConfig.TokenEndpoint,
		Scopes:       scopes,
	}

	return clientCredentialsConf.TokenSource(ctx), nil
}
