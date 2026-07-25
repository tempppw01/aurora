package bootstrap

import (
	"os"
	"path/filepath"
	"testing"

	"aurora/internal/accounts"
)

func TestLoadDisabledManagedAccounts(t *testing.T) {
	dir := t.TempDir()
	credential := "device-credential"
	id := managedAccountID("free", credential)
	path := filepath.Join(dir, "account_metadata.json")
	if err := os.WriteFile(path, []byte(`{"`+id+`":{"status":"disabled"},"active":{"status":"active"}}`), 0600); err != nil {
		t.Fatal(err)
	}

	disabled := loadDisabledManagedAccounts(path)
	if !disabled[id] || disabled["active"] {
		t.Fatalf("disabled accounts = %#v", disabled)
	}

	acct := accounts.NewAccount("test", accounts.TypeNoAuth, credential)
	acct.Status = accounts.StatusActive
	restoreDisabledStatus(acct, disabled, "free", credential)
	if acct.Status != accounts.StatusDisabled {
		t.Fatalf("status = %s, want disabled", acct.Status)
	}
}
