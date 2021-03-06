package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	stdjwt "github.com/dgrijalva/jwt-go"
	"golang.org/x/oauth2/jwt"
	"io/ioutil"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/grepplabs/kafka-proxy/pkg/apis"
	"github.com/grepplabs/kafka-proxy/pkg/libs/util"
	"github.com/grepplabs/kafka-proxy/plugin/token-info/shared"
	"github.com/hashicorp/go-plugin"
	"github.com/sirupsen/logrus"
)

const (
	StatusOK                      = 0
	StatusEmptyToken              = 1
	StatusParseJWTFailed          = 2
	StatusWrongAlgorithm          = 3
	StatusUnauthorized            = 4
	StatusNoIssueTimeInToken      = 5
	StatusNoExpirationTimeInToken = 6
	StatusTokenTooEarly           = 7
	StatusTokenExpired            = 8

	AlgorithmNone = "none"
)

var (
	clockSkew = 1 * time.Minute
)

type UnsecuredJWTVerifier struct {
	claimSub  map[string]struct{}
	algorithm map[string]struct{}
}

type pluginMeta struct {
	claimSub  util.ArrayFlags
	algorithm util.ArrayFlags
}

func (f *pluginMeta) flagSet() *flag.FlagSet {
	fs := flag.NewFlagSet("unsecured-jwt-info info settings", flag.ContinueOnError)
	fs.Var(&f.claimSub, "claim-sub", "Allowed subject claim (user name)")
	fs.Var(&f.algorithm, "algorithm", "Allowed algorithm")
	return fs
}

// Implements apis.TokenInfo
func (v UnsecuredJWTVerifier) VerifyToken(ctx context.Context, request apis.VerifyRequest) (apis.VerifyResponse, error) {
	logrus.Printf("Verifying Token %s", request.Token)
	if request.Token == "" {
		return getVerifyResponseResponse(StatusEmptyToken)
	}

	header, claimSet, err := Decode(request.Token)
	if err != nil {
		return getVerifyResponseResponse(StatusParseJWTFailed)
	}
	if len(v.algorithm) != 0 {
		if _, ok := v.algorithm[header.Algorithm]; !ok {
			return getVerifyResponseResponse(StatusUnauthorized)
		}
	}
	if len(v.claimSub) != 0 {
		if _, ok := v.claimSub[claimSet.Sub]; !ok {
			return getVerifyResponseResponse(StatusUnauthorized)
		}
	}
	if claimSet.Iat < 1 {
		return getVerifyResponseResponse(StatusNoIssueTimeInToken)
	}
	if claimSet.Exp < 1 {
		return getVerifyResponseResponse(StatusNoExpirationTimeInToken)
	}

	if claimSet.Iss != "" {
		logrus.Printf("Issuer URL is %s, trying to retrieve validation certificate", claimSet.Iss)
		cert, _, err := getKeycloakValidationCertificate(claimSet.Iss)
		if err != nil {
			logrus.Errorf("Error \"%v\" getting validation certificate", err)
		} else {
			logrus.Printf("Certificate: %s", cert)
			stdjwt.Parse()
		}
	} else {
		logrus.Errorf("Issuer URL is empty")
	}

	earliest := int64(claimSet.Iat) - int64(clockSkew.Seconds())
	latest := int64(claimSet.Exp) + int64(clockSkew.Seconds())
	unix := time.Now().Unix()

	if unix < earliest {
		return getVerifyResponseResponse(StatusTokenTooEarly)
	}
	if unix > latest {
		return getVerifyResponseResponse(StatusTokenExpired)
	}
	return getVerifyResponseResponse(StatusOK)
}

type ValidationKey struct {
	KeyId     string   `json:"kid"`
	KeyType   string   `json:"kty"`
	Algorithm string   `json:"alg"`
	Use       string   `json:"use"`
	N         string   `json:"n"`
	E         string   `json:"e"`
	X509Cert  []string `json:"x5c"`
	X5t       string   `json:"x5t"`
	X5t_S256  string   `json:"x5t#S256"`
}

type ValidationKeys struct {
	Keys []ValidationKey `json:"keys"`
}

func getKeycloakValidationCertificate(url string) (string, string, error) {
	const subpath = "protocol/openid-connect/certs"
	url = strings.Replace(url, "localhost", "host.docker.internal", 1)
	response, err := http.Get(url + "/" + subpath)
	if err != nil {
		return "", "", err
	}

	responseData, err := ioutil.ReadAll(response.Body)
	if err != nil {
		return "", "", err
	}

	keys := ValidationKeys{}
	err = json.NewDecoder(bytes.NewBuffer(responseData)).Decode(&keys)
	if err != nil {
		return "", "", err
	}

	if len(keys.Keys) == 0 {
		return "", "", fmt.Errorf("Keycloak Response contains no keys")
	}

	if len(keys.Keys[0].X509Cert) == 0 {
		return "", "", fmt.Errorf("Keycloak validation key contains nur X.509 Certificates")
	}
	return keys.Keys[0].X509Cert[0], "", nil
}

func keyFunc(keys ValidationKeys, token *stdjwt.Token) (interface{}, error) {
	validationKey := keys.Keys[0]
	if keyId, ok := token.Header["kid"]; ok {
		if k, err := getKeyById(keys, keyId); err == nil {
			validationKey = *k
		} else {
			return nil, err
		}
	}
}

func getKeyById(keys ValidationKeys, keyId interface{}) (*ValidationKey, error) {
	// If Key ID is given but key is not found, return invalid index
	for _, key := range keys.Keys {
		if key.KeyId == keyId {
			return &key, nil
		}
	}

	return nil, fmt.Errorf("no key with ID %v found", keyId)
}

type Header struct {
	Algorithm string `json:"alg"`
}

// kafka client sends float instead of int
type ClaimSet struct {
	Sub         string                 `json:"sub,omitempty"`
	Exp         float64                `json:"exp"`
	Iat         float64                `json:"iat"`
	Iss         string                 `json:"iss,omitempty"`
	OtherClaims map[string]interface{} `json:"-"`
}

func Decode(token string) (*Header, *ClaimSet, error) {
	args := strings.Split(token, ".")
	if len(args) < 2 {
		return nil, nil, errors.New("jws: invalid token received")
	}
	decodedHeader, err := base64.RawURLEncoding.DecodeString(args[0])
	if err != nil {
		return nil, nil, err
	}
	decodedPayload, err := base64.RawURLEncoding.DecodeString(args[1])
	if err != nil {
		return nil, nil, err
	}

	header := &Header{}
	err = json.NewDecoder(bytes.NewBuffer(decodedHeader)).Decode(header)
	if err != nil {
		return nil, nil, err
	}
	claimSet := &ClaimSet{}
	err = json.NewDecoder(bytes.NewBuffer(decodedPayload)).Decode(claimSet)
	if err != nil {
		return nil, nil, err
	}
	return header, claimSet, nil
}

func getVerifyResponseResponse(status int) (apis.VerifyResponse, error) {
	success := status == StatusOK
	return apis.VerifyResponse{Success: success, Status: int32(status)}, nil
}

func main() {
	pluginMeta := &pluginMeta{}
	fs := pluginMeta.flagSet()
	_ = fs.Parse(os.Args[1:])

	logrus.Infof("Unsecured JWT sub claims: %v", pluginMeta.claimSub)

	unsecuredJWTVerifier := &UnsecuredJWTVerifier{
		claimSub:  pluginMeta.claimSub.AsMap(),
		algorithm: pluginMeta.algorithm.AsMap(),
	}

	plugin.Serve(&plugin.ServeConfig{
		HandshakeConfig: shared.Handshake,
		Plugins: map[string]plugin.Plugin{
			"unsecuredJWTInfo": &shared.TokenInfoPlugin{Impl: unsecuredJWTVerifier},
		},
		// A non-nil value here enables gRPC serving for this plugin...
		GRPCServer: plugin.DefaultGRPCServer,
	})
}
