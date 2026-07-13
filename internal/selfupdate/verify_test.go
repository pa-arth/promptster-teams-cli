package selfupdate

import (
	"strings"
	"testing"
)

// sampleSig is a real minisign signature over sampleSums, produced by the
// minisign CLI with the SAME secret key whose public half is embedded in
// verify.go (minisignPublicKey). If the embedded key ever changes, regenerate
// this fixture: `minisign -S -s <secret> -m SHA256SUMS`.
const sampleSig = "untrusted comment: signature from minisign secret key\n" +
	"RUSPnqFjEKp3+SyUZi2Ddt5pJ4tfXn9IHHw88wleKflXqLc03u1lIe6x/4DQ7s5u1m5jNUy6cvmFQdYlUQ6HOQKkcLVgi0BjWAU=\n" +
	"trusted comment: timestamp:1783903247\tfile:TESTSUMS\thashed\n" +
	"Z4N3VP3vG0cM2h85ByruU7uFPISybmjBWeG8QwnNxOm56esEy4v5jtbdI9JSbi0W68dr+FrjnqWlu0AUYBKCAw=="

func TestVerifyMinisignAccept(t *testing.T) {
	if err := verifyMinisign([]byte(sampleSums), []byte(sampleSig)); err != nil {
		t.Fatalf("valid signature over sampleSums should verify: %v", err)
	}
}

func TestVerifyMinisignRejectTamperedContent(t *testing.T) {
	tampered := strings.Replace(sampleSums, "d3d9", "0000", 1)
	if err := verifyMinisign([]byte(tampered), []byte(sampleSig)); err == nil {
		t.Fatal("signature must NOT verify against tampered SHA256SUMS")
	}
}

func TestVerifyMinisignRejectTamperedSignature(t *testing.T) {
	// Flip a byte in the signature payload line.
	lines := strings.Split(sampleSig, "\n")
	if strings.HasPrefix(lines[1], "R") {
		lines[1] = "X" + lines[1][1:]
	}
	bad := strings.Join(lines, "\n")
	if err := verifyMinisign([]byte(sampleSums), []byte(bad)); err == nil {
		t.Fatal("corrupted signature must be rejected")
	}
}

func TestVerifyMinisignRejectEmpty(t *testing.T) {
	if err := verifyMinisign([]byte(sampleSums), []byte("")); err == nil {
		t.Fatal("empty signature must be rejected")
	}
}
