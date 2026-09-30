import pathlib
import sqlite3
import subprocess
import sys
import tempfile


def check(condition, message):
    if not condition:
        raise AssertionError(message)


def run(*args):
    return subprocess.run([sys.executable, "ledger.py", *map(str, args)], capture_output=True, text=True, timeout=5)


def rows(database):
    with sqlite3.connect(database) as connection:
        return connection.execute("SELECT entry_id, memo, cents FROM entries ORDER BY entry_id").fetchall()


try:
    check(pathlib.Path("test_ledger.py").is_file(), "visible tests removed")
    visible = subprocess.run([sys.executable, "-m", "unittest", "-q"], capture_output=True, text=True, timeout=5)
    check(visible.returncode == 0, f"visible baseline tests regressed: {visible.stderr[-500:]}")
    with tempfile.TemporaryDirectory() as directory:
        root = pathlib.Path(directory)
        database = root / "ledger.db"
        source = root / "first.csv"
        source.write_text('entry_id,memo,amount\na1,"tea, cups",0.29\na2,refill,1.01\n', encoding="utf-8")
        check(run("import", database, source).returncode == 0, "quoted CSV import failed")
        expected = [("a1", "tea, cups", 29), ("a2", "refill", 101)]
        check(rows(database) == expected, "CSV or integer-cents content wrong")
        check(run("summary", database).stdout.strip() == "2 130", "summary regressed")
        check(run("import", database, source).returncode == 0, "repeat import failed")
        check(rows(database) == expected, "repeat import duplicated rows")
        bad = root / "bad.csv"
        bad.write_text("entry_id,memo,amount\na3,new,2.00\na4,bad,0.001\n", encoding="utf-8")
        check(run("import", database, bad).returncode != 0, "invalid amount accepted")
        check(rows(database) == expected, "failed batch partially committed")
        conflict = root / "conflict.csv"
        conflict.write_text("entry_id,memo,amount\na3,new,2.00\na1,changed,0.29\n", encoding="utf-8")
        check(run("import", database, conflict).returncode != 0, "conflicting ID accepted")
        check(rows(database) == expected, "conflicting batch changed prior rows")
    print("ledger: passed")
except (AssertionError, OSError, sqlite3.Error, subprocess.TimeoutExpired) as error:
    print(f"ledger: {error}", file=sys.stderr)
    raise SystemExit(1) from error
