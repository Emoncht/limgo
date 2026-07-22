package totp

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"strings"
	"time"
)

// GenerateOTP generates a 6-digit TOTP token using HMAC-SHA1 with a 180-second step.
func GenerateOTP(secret string) (string, error) {
	// Normalize secret: uppercase and strip spaces
	secret = strings.ToUpper(strings.ReplaceAll(secret, " ", ""))
	
	// Ensure base32 padding if needed
	if missingPadding := len(secret) % 8; missingPadding != 0 {
		secret += strings.Repeat("=", 8-missingPadding)
	}

	key, err := base32.StdEncoding.DecodeString(secret)
	if err != nil {
		return "", fmt.Errorf("failed to decode base32 secret: %w", err)
	}

	// Step size 180s
	counter := uint64(time.Now().Unix() / 180)

	buf := make([]byte, 8)
	binary.BigEndian.PutUint64(buf, counter)

	h := hmac.New(sha1.New, key)
	h.Write(buf)
	hash := h.Sum(nil)

	offset := hash[len(hash)-1] & 0x0f
	binaryVal := (uint32(hash[offset]&0x7f) << 24) |
		(uint32(hash[offset+1]) << 16) |
		(uint32(hash[offset+2]) << 8) |
		(uint32(hash[offset+3]))

	otp := binaryVal % 1000000
	return fmt.Sprintf("%06d", otp), nil
}
