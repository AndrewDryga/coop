#!/bin/sh
# Turns a title into a url slug:  "Hello There"  ->  "hello-there"
printf '%s' "$1" | tr '[:upper:]' '[:lower:]' | tr ' ' '-'
