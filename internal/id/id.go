// Package id generates ACP-format identifiers.
package id

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

func random8() string {
	b := make([]byte, 4)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func uuidV4() string {
	b := make([]byte, 16)
	rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

func Covenant() string  { return "cvnt_" + uuidV4() }
func Agent() string     { return "agent_" + random8() }
func Platform() string  { return "pid_" + random8() }
func LogID() string     { return uuidV4() }
func LedgerID() string  { return uuidV4() }
func SettlementID() string { return "sout_" + random8() }
func SessionID() string { return uuidV4() }
func AnchorID() string  { return "anch_" + random8() }
func AccessRequest() string { return "areq_" + random8() }
