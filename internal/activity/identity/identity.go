package identity

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	IdentityIssuer = "streamy-activity"
	TokenExpiry    = 30 * 24 * time.Hour // 30 days
)

var (
	ErrInvalidSecret   = errors.New("DISCORD_IDENTITY_SECRET must be at least 32 characters")
	ErrInvalidToken    = errors.New("invalid identity token")
	ErrTokenExpired    = errors.New("identity token expired")
	ErrInvalidIssuer   = errors.New("invalid token issuer")
	ErrInvalidClaims   = errors.New("invalid token claims")
)

type Claims struct {
	Issuer    string `json:"iss"`
	Subject   string `json:"sub"`
	Name      string `json:"name"`
	Avatar    string `json:"avatar"`
	IssuedAt  int64  `json:"iat"`
	ExpiresAt int64  `json:"exp"`
}

type DiscordUser struct {
	ID        string `json:"id"`
	Username  string `json:"username"`
	GlobalName string `json:"global_name"`
	Avatar    string `json:"avatar"`
}

func base64URLEncode(data []byte) string {
	return strings.TrimRight(base64.RawURLEncoding.EncodeToString(data), "=")
}

func base64URLDecode(s string) ([]byte, error) {
	// Add padding if needed
	switch len(s) % 4 {
	case 2:
		s += "=="
	case 3:
		s += "="
	}
	return base64.RawURLEncoding.DecodeString(s)
}

func SigningKey(secret string) ([]byte, error) {
	if len(secret) < 32 {
		return nil, ErrInvalidSecret
	}
	return []byte(secret), nil
}

func signHMAC(key, data []byte) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write(data)
	return mac.Sum(nil)
}

func verifyHMAC(key, data, sig []byte) bool {
	expected := signHMAC(key, data)
	return hmac.Equal(expected, sig)
}

// IssueIdentityToken creates a JWT token for a Discord user
func IssueIdentityToken(user DiscordUser, secret string) (string, error) {
	key, err := SigningKey(secret)
	if err != nil {
		return "", err
	}

	now := time.Now()
	avatarURL := ""
	if user.Avatar != "" {
		avatarURL = fmt.Sprintf("https://cdn.discordapp.com/avatars/%s/%s.png?size=128", user.ID, user.Avatar)
	}

	name := user.GlobalName
	if name == "" {
		name = user.Username
	}

	claims := Claims{
		Issuer:    IdentityIssuer,
		Subject:   user.ID,
		Name:      name,
		Avatar:    avatarURL,
		IssuedAt:  now.Unix(),
		ExpiresAt: now.Add(TokenExpiry).Unix(),
	}

	header := map[string]string{"alg": "HS256", "typ": "JWT"}
	headerJSON, _ := json.Marshal(header)
	claimsJSON, _ := json.Marshal(claims)

	headerB64 := base64URLEncode(headerJSON)
	claimsB64 := base64URLEncode(claimsJSON)

	data := headerB64 + "." + claimsB64
	sig := signHMAC(key, []byte(data))
	sigB64 := base64URLEncode(sig)

	return data + "." + sigB64, nil
}

// VerifyIdentityToken validates a JWT token and returns the claims
func VerifyIdentityToken(token, secret string) (*Claims, error) {
	key, err := SigningKey(secret)
	if err != nil {
		return nil, err
	}

	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, ErrInvalidToken
	}

	data := parts[0] + "." + parts[1]
	sig, err := base64URLDecode(parts[2])
	if err != nil {
		return nil, ErrInvalidToken
	}

	if !verifyHMAC(key, []byte(data), sig) {
		return nil, ErrInvalidToken
	}

	claimsJSON, err := base64URLDecode(parts[1])
	if err != nil {
		return nil, ErrInvalidToken
	}

	var claims Claims
	if err := json.Unmarshal(claimsJSON, &claims); err != nil {
		return nil, ErrInvalidToken
	}

	// Validate claims
	if claims.Issuer != IdentityIssuer {
		return nil, ErrInvalidIssuer
	}
	if claims.Subject == "" {
		return nil, ErrInvalidClaims
	}
	if claims.Name == "" || len(claims.Name) > 80 {
		return nil, ErrInvalidClaims
	}
	if claims.Avatar != "" && len(claims.Avatar) > 512 {
		return nil, ErrInvalidClaims
	}
	if claims.ExpiresAt <= time.Now().Unix() {
		return nil, ErrTokenExpired
	}

	return &claims, nil
}

// ValidLoopbackCallback validates the OAuth callback URL format
func ValidLoopbackCallback(value string) bool {
	// Must be http://127.0.0.1:<port>/api/discord/callback
	// with no query, fragment, userinfo
	if !strings.HasPrefix(value, "http://127.0.0.1:") {
		return false
	}
	// Check path
	if !strings.HasSuffix(value, "/api/discord/callback") {
		return false
	}
	// No query or fragment
	if strings.Contains(value, "?") || strings.Contains(value, "#") {
		return false
	}
	// Extract port
	portPart := strings.TrimPrefix(value, "http://127.0.0.1:")
	portPart = strings.TrimSuffix(portPart, "/api/discord/callback")
	if portPart == "" {
		return false
	}
	// Port should be numeric
	for _, c := range portPart {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}