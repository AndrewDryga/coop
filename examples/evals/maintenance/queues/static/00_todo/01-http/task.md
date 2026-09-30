# Preserve HTTP query and HEAD semantics

Serve `/index.html?cache=1` as the same file as `/index.html`. A `HEAD` request must return the GET status and relevant headers, including the representation's byte length, without a body. Preserve ordinary GET behavior and add visible tests.

Not done: treating the query as part of the filename or sending a body for HEAD.
