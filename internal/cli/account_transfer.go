package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/ui"
)

func (a *app) accountTransfer(provider, account, action string) (int, error) {
	if !validProfileName(account) {
		return 2, &ui.UsageError{
			Headline: fmt.Sprintf("Invalid account name %q", account),
			Cause:    "Use a single directory name for the account.",
			Rows:     [][2]string{{"Help:", "coop help credentials transfer"}},
		}
	}
	if controlled, _ := box.ControllerModelAccount(context.Background(), provider, account); controlled {
		return 1, errors.New("manage accounts on the installation that owns the sign-in")
	}
	if action == "export" {
		fmt.Fprintln(os.Stderr, "Paste the receive code from the destination you control, then press Enter.")
		reader := bufio.NewReaderSize(os.Stdin, 4096)
		codeBytes, err := reader.ReadSlice('\n')
		code := string(codeBytes)
		if err != nil && err != io.EOF {
			return 1, err
		}
		sealed, description, err := box.ExportAccount(context.Background(), a.cfg, provider, account, strings.TrimSpace(code), func(target string) error {
			fmt.Fprintf(os.Stderr, "Move renewal to %s and sign this source account out? Type MOVE to continue.\n", target)
			line, err := reader.ReadSlice('\n')
			if err != nil && err != io.EOF || strings.TrimSpace(string(line)) != "MOVE" {
				return errors.New("account move cancelled")
			}
			return nil
		})
		if err != nil {
			return 1, err
		}
		fmt.Fprintf(os.Stderr, "%s. Copy the encrypted text below to the waiting importer.\n", description)
		// Short lines survive a terminal's canonical input line limit when pasted.
		for len(sealed) > 76 {
			if _, err := fmt.Fprintln(os.Stdout, sealed[:76]); err != nil {
				return 1, err
			}
			sealed = sealed[76:]
		}
		_, err = fmt.Fprintln(os.Stdout, sealed)
		if err != nil {
			return 1, err
		}
		return 0, nil
	}
	receiver, err := box.NewAccountReceiver(context.Background(), a.cfg, provider, account)
	if err != nil {
		return 1, err
	}
	fmt.Fprintf(os.Stderr, "On the signed-in Coop, run coop credentials %s <source-account> export and paste this receive code. Keep this importer open.\n", provider)
	if _, err = fmt.Fprintln(os.Stdout, receiver.Code); err != nil {
		return 1, err
	}
	fmt.Fprintln(os.Stderr, "Paste the encrypted account bundle here, then press Enter on an empty line.")
	reader := bufio.NewReaderSize(os.Stdin, 4096)
	for {
		var bundle strings.Builder
		total := 0
		for {
			buf, readErr := reader.ReadSlice('\n')
			line := string(buf)
			if readErr == bufio.ErrBufferFull {
				readErr = nil
			}
			total += len(line)
			if total > box.AccountBundleLimit*2 {
				return 1, errors.New("account bundle is too large")
			}
			line = strings.TrimSpace(line)
			if bundle.Len()+len(line) > box.AccountBundleLimit {
				return 1, errors.New("account bundle is too large")
			}
			bundle.WriteString(line)
			if readErr != nil && readErr != io.EOF {
				return 1, readErr
			}
			if line == "" || readErr == io.EOF {
				break
			}
		}
		if bundle.Len() == 0 {
			return 1, errors.New("no account bundle was pasted")
		}
		alias := account
		err = receiver.Import(context.Background(), a.cfg, provider, account, bundle.String())
		for errors.Is(err, box.ErrImportOccupied) {
			fmt.Fprintln(os.Stderr, "This account name already has access. Enter an unused local name for the incoming account, or Ctrl-C to cancel.")
			line, readErr := reader.ReadSlice('\n')
			if readErr != nil && readErr != io.EOF {
				return 1, readErr
			}
			alias = strings.TrimSpace(string(line))
			if !validProfileName(alias) {
				return 1, errors.New("choose an unused local account name")
			}
			err = receiver.ImportAs(context.Background(), a.cfg, provider, account, alias, bundle.String())
		}
		if err == nil {
			return finishLogin(a.cfg, provider, alias)
		}
		if !ui.IsTerminal(os.Stdin) {
			return 1, err
		}
		fmt.Fprintln(os.Stderr, err)
		fmt.Fprintln(os.Stderr, "Paste the bundle again, or press Ctrl-C to cancel.")
	}
}
