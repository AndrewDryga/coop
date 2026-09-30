import pathlib
import sqlite3
import tempfile
import unittest

import ledger


class LedgerTest(unittest.TestCase):
    def test_simple_import_and_summary_data(self):
        with tempfile.TemporaryDirectory() as directory:
            directory = pathlib.Path(directory)
            source = directory / "rows.csv"
            source.write_text("entry_id,memo,amount\na1,lunch,2.50\n", encoding="utf-8")
            database = directory / "entries.db"
            ledger.import_csv(database, source)
            with sqlite3.connect(database) as connection:
                self.assertEqual(
                    connection.execute("SELECT entry_id, memo, cents FROM entries").fetchall(),
                    [("a1", "lunch", 250)],
                )
