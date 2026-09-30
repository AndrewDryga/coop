import pathlib
import sqlite3
import tempfile
import unittest

import ledger


class LedgerTest(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.directory = pathlib.Path(temporary.name)
        self.database = self.directory / "entries.db"

    def source(self, content):
        source = self.directory / "rows.csv"
        source.write_text(content, encoding="utf-8")
        return source

    def rows(self):
        with sqlite3.connect(self.database) as connection:
            return connection.execute("SELECT entry_id, memo, cents FROM entries ORDER BY entry_id").fetchall()

    def test_simple_import_and_summary_data(self):
        ledger.import_csv(self.database, self.source("entry_id,memo,amount\na1,lunch,2.50\n"))
        self.assertEqual(self.rows(), [("a1", "lunch", 250)])

    def test_quoted_csv_exact_cents_and_repeat(self):
        source = self.source('entry_id,memo,amount\na1,"coffee, cake",0.29\n')
        ledger.import_csv(self.database, source)
        ledger.import_csv(self.database, source)
        self.assertEqual(self.rows(), [("a1", "coffee, cake", 29)])

    def test_invalid_batch_rolls_back(self):
        ledger.import_csv(self.database, self.source("entry_id,memo,amount\na1,lunch,2.50\n"))
        with self.assertRaises(ValueError):
            ledger.import_csv(self.database, self.source("entry_id,memo,amount\na2,new,1.00\na3,bad,0.001\n"))
        self.assertEqual(self.rows(), [("a1", "lunch", 250)])

    def test_conflicting_batch_rolls_back(self):
        ledger.import_csv(self.database, self.source("entry_id,memo,amount\na1,lunch,2.50\n"))
        with self.assertRaises(ValueError):
            ledger.import_csv(self.database, self.source("entry_id,memo,amount\na2,new,1.00\na1,changed,2.50\n"))
        self.assertEqual(self.rows(), [("a1", "lunch", 250)])
