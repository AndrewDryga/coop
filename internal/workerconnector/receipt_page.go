package workerconnector

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/AndrewDryga/coop/internal/workerproto"
)

// Raw JSON must retain its bounded representation through custody and delivery. HTML/JavaScript
// escaping is unnecessary on this private JSON transport and can multiply a valid payload's size.
// Identity digests and replay comparisons deliberately keep their existing canonical encoder.
func encodeWireJSON(value any) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return buffer.Bytes()[:buffer.Len()-1], nil
}

type receiptPage struct {
	poll              workerproto.Poll
	cursor            string
	issue             error
	settlementPending bool
}

func (j *journal) nextReceiptPage(base workerproto.Poll) (receiptPage, error) {
	page := receiptPage{poll: base}
	if err := base.Validate(); err != nil {
		return page, fmt.Errorf("validate outbound worker hello: %w", err)
	}
	if !pollFits(base) {
		return page, errors.New("worker hello exceeds transport bound")
	}
	entries, err := j.pending()
	if err != nil {
		return page, err
	}
	if cursor, err := os.ReadFile(j.receiptScanPath()); err == nil {
		if !reference(string(cursor), 256) {
			return page, errors.New("worker receipt scan cursor is malformed")
		}
		index := sort.Search(len(entries), func(index int) bool { return entries[index].CommandID > string(cursor) })
		entries = append(entries[index:], entries[:index]...)
	} else if !errors.Is(err, os.ErrNotExist) {
		return page, fmt.Errorf("read worker receipt scan cursor: %w", err)
	}
	for index, entry := range entries {
		if index == workerproto.MaxBatchItems {
			break
		}
		// Validate a receipt on its own first: an old unsendable result must not prevent
		// healthy siblings from settling, but its original custody must remain untouched.
		byteSize, deliverable := receiptWireSize(base, entry)
		if !deliverable {
			if page.issue == nil {
				page.issue = fmt.Errorf("worker receipt %q cannot be delivered (%d encoded bytes); custody retained", entry.CommandID, byteSize)
			}
			page.cursor = entry.CommandID
			continue
		}
		candidate := appendReceipt(page.poll, entry)
		if !pollFits(candidate) {
			// This unit fits a fresh page. Let it lead the next scan rather than
			// continually filling around a large result with smaller later receipts.
			break
		}
		page.poll, page.cursor = candidate, entry.CommandID
	}
	page.settlementPending = len(page.poll.CommandResults) > 0
	if !page.settlementPending {
		// Received receipts may occupy this page while a completed result waits on
		// the next. Only permanently unsendable custody may yield to activity.
		for _, entry := range entries {
			if entry.Result != nil {
				if _, deliverable := receiptWireSize(base, entry); deliverable {
					page.settlementPending = true
					break
				}
			}
		}
	}
	return page, nil
}

func receiptWireSize(base workerproto.Poll, entry journalEntry) (int, bool) {
	single := appendReceipt(base, entry)
	encoded, err := encodeWireJSON(single)
	identityMatches := entry.Result == nil || (entry.Result.CommandID == entry.CommandID &&
		entry.Result.OperationKey == entry.Command.IdempotencyKey)
	return len(encoded), err == nil && identityMatches && single.Validate() == nil && len(encoded) <= workerproto.MaxDocumentBytes
}

func appendReceipt(poll workerproto.Poll, entry journalEntry) workerproto.Poll {
	poll.AcknowledgedCommandIDs = append(poll.AcknowledgedCommandIDs, entry.CommandID)
	if entry.Result != nil {
		poll.CommandResults = append(poll.CommandResults, *entry.Result)
	}
	return poll
}

func (j *journal) receiptScanPath() string { return filepath.Join(j.dir, ".receipt-scan") }

func (j *journal) advanceReceiptScan(commandID string) error {
	if commandID == "" {
		return nil
	}
	if !reference(commandID, 256) {
		return errors.New("worker receipt scan cursor is malformed")
	}
	temporary, err := os.CreateTemp(j.dir, ".receipt-scan-*")
	if err != nil {
		return fmt.Errorf("create worker receipt scan cursor: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.WriteString(commandID); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, j.receiptScanPath()); err != nil {
		return err
	}
	return syncDir(j.dir)
}
