package main

import (
	"flag"
	"fmt"

	"github.com/00101010xyz/mcpaw/internal/secrets"
)

// runKeygen prints a fresh base64-encoded master key.
//
// It exists so an operator can generate MCPAW_MASTER_KEY once and pin it in
// their deployment's secret store, rather than relying on the generated key
// file that Load-or-create leaves under DataDir — the explicit path is easier
// to back up and to rotate deliberately.
func runKeygen(args []string) error {
	fs := flag.NewFlagSet("keygen", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	key, err := secrets.GenerateMasterKey()
	if err != nil {
		return fmt.Errorf("generating master key: %w", err)
	}
	fmt.Println(key)
	return nil
}
