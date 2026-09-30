import csv
from decimal import Decimal
import re
import sqlite3
import sys


AMOUNT = re.compile(r"-?[0-9]+(?:\.[0-9]{1,2})?")


def import_csv(db_path, csv_path):
    connection = sqlite3.connect(db_path)
    try:
        with connection:
            connection.execute(
                "CREATE TABLE IF NOT EXISTS entries "
                "(entry_id TEXT PRIMARY KEY, memo TEXT NOT NULL, cents INTEGER NOT NULL)"
            )
            with open(csv_path, newline="", encoding="utf-8") as source:
                reader = csv.DictReader(source)
                if reader.fieldnames != ["entry_id", "memo", "amount"]:
                    raise ValueError("invalid CSV header")
                for row in reader:
                    if None in row or not row["entry_id"] or row["memo"] is None:
                        raise ValueError("malformed row")
                    amount = row["amount"]
                    if amount is None or not AMOUNT.fullmatch(amount):
                        raise ValueError("invalid amount")
                    cents = int(Decimal(amount) * 100)
                    item = (row["entry_id"], row["memo"], cents)
                    prior = connection.execute(
                        "SELECT memo, cents FROM entries WHERE entry_id = ?", (item[0],)
                    ).fetchone()
                    if prior is None:
                        connection.execute("INSERT INTO entries VALUES (?, ?, ?)", item)
                    elif prior != item[1:]:
                        raise ValueError("conflicting entry_id")
    finally:
        connection.close()


def summary(db_path):
    with sqlite3.connect(db_path) as connection:
        count, total = connection.execute("SELECT COUNT(*), COALESCE(SUM(cents), 0) FROM entries").fetchone()
    print(f"{count} {total}")


if __name__ == "__main__":
    try:
        if len(sys.argv) == 4 and sys.argv[1] == "import":
            import_csv(sys.argv[2], sys.argv[3])
        elif len(sys.argv) == 3 and sys.argv[1] == "summary":
            summary(sys.argv[2])
        else:
            raise ValueError("usage: ledger.py import <database> <csv> | summary <database>")
    except (OSError, ValueError, sqlite3.Error) as error:
        raise SystemExit(str(error)) from error
