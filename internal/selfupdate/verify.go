package selfupdate

import (
	"errors"

	minisign "github.com/jedisct1/go-minisign"
)

// minisignPublicKey is the EMBEDDED trust root for auto-update. It is the raw
// base64 payload line (not the two-line .pub file) of the keypair whose secret
// half signs SHA256SUMS in .github/workflows/release.yml. Baking it into the
// binary — rather than fetching it — is the whole point: an attacker who
// controls the release host or the network still cannot forge an update without
// this private key, because every candidate SHA256SUMS is checked against this
// constant before its checksums are trusted. The matching minisign.pub lives at
// the repo root for humans; this const is the source of truth at runtime.
//
// Key id: F977AA1063A19E8F.
const minisignPublicKey = "RWSPnqFjEKp3+YGItrZaM+Ks6clhwDqFJBSDO/rMU1/KTm7xuijKxmO2"

// verifyMinisign checks that sig is a valid minisign signature over sums under
// the embedded public key. It returns nil only on a cryptographically valid
// signature; every other path (bad key parse, malformed signature, wrong key
// id, failed verification) returns an error so the caller REJECTS the update.
// An unsigned or absent signature never reaches here as "valid" — the caller
// treats a missing .minisig as a hard failure before calling this.
func verifyMinisign(sums, sig []byte) error {
	pk, err := minisign.NewPublicKey(minisignPublicKey)
	if err != nil {
		return err
	}
	signature, err := minisign.DecodeSignature(string(sig))
	if err != nil {
		return err
	}
	ok, err := pk.Verify(sums, signature)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("selfupdate: minisign signature did not verify")
	}
	return nil
}
