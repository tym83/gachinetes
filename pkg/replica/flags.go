package replica

import (
	"fmt"
	"os"
	"strings"

	"filippo.io/age"
)

// Recipients reads age recipients: public keys, one per line, from a file
// (comments and blank lines skipped), and given one by one.
func Recipients(file string, keys []string) ([]age.Recipient, error) {
	var text strings.Builder
	if file != "" {
		raw, err := os.ReadFile(file)
		if err != nil {
			return nil, err
		}
		text.Write(raw)
		text.WriteString("\n")
	}
	for _, k := range keys {
		text.WriteString(k + "\n")
	}
	if strings.TrimSpace(text.String()) == "" {
		return nil, nil
	}
	rs, err := age.ParseRecipients(strings.NewReader(text.String()))
	if err != nil {
		return nil, fmt.Errorf("age recipients: %w", err)
	}
	return rs, nil
}

// Identities reads age identities (secret keys) from a file.
func Identities(file string) ([]age.Identity, error) {
	if file == "" {
		return nil, nil
	}
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	ids, err := age.ParseIdentities(f)
	if err != nil {
		return nil, fmt.Errorf("age identities in %s: %w", file, err)
	}
	return ids, nil
}
