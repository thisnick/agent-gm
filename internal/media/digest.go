package media

// `sha256` on an attachment is the digest of the **decrypted** bytes,
// computed on demand and cached (spec section 10.1). Google carries a digest
// of the ciphertext, which is not what an agent comparing its downloaded copy
// would compute, so serving Google's value would be worse than serving none:
// it would look like an answer and never match.
//
// When it cannot be computed, `sha256` is null, `sha256_available` is false,
// and `sha256_unavailable_reason` is one of the three below -- which also
// appears in `warnings`, so a caller reading only the envelope still learns
// that a field it might have compared is absent for a reason.

// The closed vocabulary of spec section 10.1.
const (
	// ReasonLargerThanCacheBudget: the object is bigger than
	// `media.cache_max_bytes`, so hashing it would mean holding it entirely
	// outside the cache the budget exists to bound.
	ReasonLargerThanCacheBudget = "larger_than_cache_budget"
	// ReasonDownloadBudgetExhausted: the concurrent-download allowance of
	// spec section 12.3 is spent, and a metadata read must not queue behind
	// bytes.
	ReasonDownloadBudgetExhausted = "download_budget_exhausted"
	// ReasonBytesUnavailable: the bytes are not here and cannot be had --
	// the attachment is still `pending`, the download failed, or Google no
	// longer has it.
	ReasonBytesUnavailable = "bytes_unavailable"
)

// SHA256UnavailableReasons is the vocabulary, for a test that walks it.
func SHA256UnavailableReasons() []string {
	return []string{
		ReasonLargerThanCacheBudget,
		ReasonDownloadBudgetExhausted,
		ReasonBytesUnavailable,
	}
}

// ValidSHA256UnavailableReason reports whether r is one of the three. It
// exists so a handler cannot invent a fourth: the field is a closed
// vocabulary and a caller branching on it should never meet a value the
// documentation does not list.
func ValidSHA256UnavailableReason(r string) bool {
	for _, known := range SHA256UnavailableReasons() {
		if known == r {
			return true
		}
	}
	return false
}

// SHA256UnavailableWarning is the envelope warning that accompanies the
// reason: `sha256_unavailable:<reason>` (spec section 10.1).
func SHA256UnavailableWarning(reason string) string {
	return "sha256_unavailable:" + reason
}
