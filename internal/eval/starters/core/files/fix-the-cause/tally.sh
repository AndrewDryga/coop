#!/bin/sh
# Prints how many records are in records.txt.
count=0
# Read every record and count it.
while read -r line; do
	count=$((count + 1))
done < records.txt
echo "$count"
