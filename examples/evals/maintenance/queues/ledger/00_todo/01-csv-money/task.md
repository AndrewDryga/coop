# Parse CSV and money accurately

Use Python's CSV parser for `entry_id,memo,amount` rows, including quoted commas in memos. Parse decimal dollar amounts into exact integer cents without binary floating-point rounding. Reject malformed rows and amounts with more than two fractional digits. Keep the simple import and summary CLI working. Add visible tests.

Not done: splitting on commas or storing floating-point cents.
