# CSV ledger

`python3 ledger.py import <database> <csv>` imports rows with `entry_id,memo,amount` columns; amounts are dollars and the database stores integer cents. `python3 ledger.py summary <database>` prints `<count> <total-cents>`.

The queue fixes CSV quoting and decimal accuracy, makes a batch all-or-nothing, and permits safe repeated imports. Run `python3 -m unittest -q`.
