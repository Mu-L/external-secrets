/*
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

package github

import (
	"bytes"
	"context"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/golang-jwt/jwt/v5"
	corev1 "k8s.io/api/core/v1"
	apiextensions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	genv1alpha1 "github.com/external-secrets/external-secrets/apis/generators/v1alpha1"
)

type Generator struct {
	httpClient *http.Client
}

type Github struct {
	HTTP         *http.Client
	Kube         client.Client
	Namespace    string
	URL          string
	InstallTkn   string
	Repositories []string
	Permissions  map[string]string
}

const (
	defaultLoginUsername = "token"
	defaultGithubAPI     = "https://api.github.com"

	errNoSpec    = "no config spec provided"
	errParseSpec = "unable to parse spec: %w"
	errGetToken  = "unable to get authorization token: %w"

	contextTimeout    = 30 * time.Second
	httpClientTimeout = 5 * time.Second
)

func (g *Generator) Generate(ctx context.Context, jsonSpec *apiextensions.JSON, kube client.Client, namespace string) (map[string][]byte, genv1alpha1.GeneratorProviderState, error) {
	return g.generate(
		ctx,
		jsonSpec,
		kube,
		namespace,
	)
}

func (g *Generator) Cleanup(ctx context.Context, jsonSpec *apiextensions.JSON, _ genv1alpha1.GeneratorProviderState, crClient client.Client, namespace string) error {
	return nil
}

func (g *Generator) generate(
	ctx context.Context,
	jsonSpec *apiextensions.JSON,
	kube client.Client,
	namespace string) (map[string][]byte, genv1alpha1.GeneratorProviderState, error) {
	if jsonSpec == nil {
		return nil, nil, errors.New(errNoSpec)
	}
	ctx, cancel := context.WithTimeout(ctx, contextTimeout)
	defer cancel()

	gh, err := newGHClient(ctx, kube, namespace, g.httpClient, jsonSpec)
	if err != nil {
		return nil, nil, fmt.Errorf("error creating request: %w", err)
	}

	payload := make(map[string]interface{})
	if gh.Permissions != nil {
		payload["permissions"] = gh.Permissions
	}
	if len(gh.Repositories) > 0 {
		payload["repositories"] = gh.Repositories
	}

	var body io.Reader = http.NoBody
	if len(payload) > 0 {
		bodyBytes, err := json.Marshal(payload)
		if err != nil {
			return nil, nil, fmt.Errorf("error marshaling payload: %w", err)
		}

		body = bytes.NewReader(bodyBytes)
	}

	// Github api expects POST request
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, gh.URL, body)
	if err != nil {
		return nil, nil, fmt.Errorf("error creating request: %w", err)
	}
	req.Header.Add("Authorization", "Bearer "+gh.InstallTkn)
	req.Header.Add("Accept", "application/vnd.github.v3+json")

	resp, err := gh.HTTP.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("error performing request: %w", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	// git access token
	var gat map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&gat); err != nil && resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil, nil, fmt.Errorf("error decoding response: %w", err)
	}

	if resp.StatusCode >= 300 {
		if message, ok := gat["message"]; ok {
			return nil, nil, fmt.Errorf("error generating token: response code: %d, response: %v", resp.StatusCode, message)
		}
		return nil, nil, fmt.Errorf("error generating token, failed to extract error message from github request: response code: %d", resp.StatusCode)
	}

	accessToken, ok := gat["token"].(string)
	if !ok {
		return nil, nil, errors.New("token isn't a string or token key doesn't exist")
	}
	return map[string][]byte{
		defaultLoginUsername: []byte(accessToken),
	}, nil, nil
}

func newGHClient(ctx context.Context, k client.Client, n string, hc *http.Client,
	js *apiextensions.JSON) (*Github, error) {
	if hc == nil {
		hc = &http.Client{
			Timeout: httpClientTimeout,
		}
	}
	res, err := parseSpec(js.Raw)
	if err != nil {
		return nil, fmt.Errorf(errParseSpec, err)
	}
	gh := &Github{
		Kube:         k,
		Namespace:    n,
		HTTP:         hc,
		Repositories: res.Spec.Repositories,
		Permissions:  res.Spec.Permissions,
	}

	ghPath := fmt.Sprintf("/app/installations/%s/access_tokens", res.Spec.InstallID)
	gh.URL = defaultGithubAPI + ghPath
	if res.Spec.URL != "" {
		gh.URL = res.Spec.URL + ghPath
	}
	secret := &corev1.Secret{}
	if err := gh.Kube.Get(ctx, client.ObjectKey{Name: res.Spec.Auth.PrivateKey.SecretRef.Name, Namespace: n}, secret); err != nil {
		return nil, fmt.Errorf("error getting GH pem from secret:%w", err)
	}

	pk, err := jwt.ParseRSAPrivateKeyFromPEM(secret.Data[res.Spec.Auth.PrivateKey.SecretRef.Key])
	if err != nil {
		return nil, fmt.Errorf("error parsing RSA private key: %w", err)
	}
	if gh.InstallTkn, err = GetInstallationToken(pk, res.Spec.AppID); err != nil {
		return nil, fmt.Errorf("can't get InstallationToken: %w", err)
	}
	return gh, nil
}

// Get github installation token.
func GetInstallationToken(key *rsa.PrivateKey, aid string) (string, error) {
	claims := jwt.RegisteredClaims{
		Issuer:    aid,
		IssuedAt:  jwt.NewNumericDate(time.Now().Add(-time.Second * 10)),
		ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Second * 300)),
	}

	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	signedToken, err := token.SignedString(key)
	if err != nil {
		return "", fmt.Errorf("error signing token: %w", err)
	}

	return signedToken, nil
}

func parseSpec(data []byte) (*genv1alpha1.GithubAccessToken, error) {
	var spec genv1alpha1.GithubAccessToken
	err := yaml.Unmarshal(data, &spec)
	return &spec, err
}

func init() {
	genv1alpha1.Register(genv1alpha1.GithubAccessTokenKind, &Generator{})
}
