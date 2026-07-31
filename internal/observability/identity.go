package observability

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
)

func UserHash(secret string, telegramID int64) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(strconv.FormatInt(telegramID, 10)))
	return hex.EncodeToString(mac.Sum(nil)[:8])
}
