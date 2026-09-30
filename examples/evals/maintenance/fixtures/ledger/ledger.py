import sqlite3
import sys


def import_csv(db_path, csv_path):
    connection = sqlite3.connect(db_path)
    connection.execute("CREATE TABLE IF NOT EXISTS entries (entry_id TEXT, memo TEXT, cents INTEGER)")
    with open(csv_path, encoding="utf-8") as source:
        next(source)
        for line in source:
            entry_id, memo, amount = line.strip().split(",")
            cents = int(float(amount) * 100)
            connection.execute("INSERT INTO entries VALUES (?, ?, ?)", (entry_id, memo, cents))
            connection.commit()
    connection.close()


def summary(db_path):
    with sqlite3.connect(db_path) as connection:
        count, total = connection.execute("SELECT COUNT(*), COALESCE(SUM(cents), 0) FROM entries").fetchone()
    print(f"{count} {total}")


if __name__ == "__main__":
    if len(sys.argv) == 4 and sys.argv[1] == "import":
        import_csv(sys.argv[2], sys.argv[3])
    elif len(sys.argv) == 3 and sys.argv[1] == "summary":
        summary(sys.argv[2])
    else:
        raise SystemExit("usage: ledger.py import <database> <csv> | summary <database>")
