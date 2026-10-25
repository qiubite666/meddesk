package auth

import (
	"errors"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

// tokenTypeAccess 是访问令牌 claims 中 typ 的取值，用于区分未来的其他令牌类型。
const tokenTypeAccess = "access"

// Claims 是 MedDesk 访问令牌的载荷。
type Claims struct {
	jwt.RegisteredClaims
	// Typ 标识令牌类型，固定为 "access"。
	Typ string `json:"typ"`
}

// signAccessToken 签发 HS256 访问令牌，返回令牌串与剩余有效秒数。
func (s *Service) signAccessToken(userID string) (string, int64, error) {
	now := time.Now()
	exp := now.Add(s.cfg.AccessTTL)
	claims := Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   userID,
			ID:        uuid.NewString(), // jti
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(exp),
		},
		Typ: tokenTypeAccess,
	}
	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(s.cfg.JWTSecret))
	if err != nil {
		return "", 0, err
	}
	expiresIn := int64(time.Until(exp) / time.Second)
	if expiresIn < 0 {
		expiresIn = 0
	}
	return signed, expiresIn, nil
}

// parseAccessToken 校验签名与有效期，返回令牌载荷。
func (s *Service) parseAccessToken(tokenStr string) (*Claims, error) {
	claims := &Claims{}
	tok, err := jwt.ParseWithClaims(tokenStr, claims, func(*jwt.Token) (interface{}, error) {
		return []byte(s.cfg.JWTSecret), nil
	}, jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Name}))
	if err != nil {
		return nil, err
	}
	if !tok.Valid {
		return nil, errors.New("令牌无效")
	}
	if claims.Typ != tokenTypeAccess || claims.Subject == "" {
		return nil, errors.New("令牌类型不正确")
	}
	return claims, nil
}
