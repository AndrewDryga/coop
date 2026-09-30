# Decode requested paths

Serve a file whose name contains a space when requested through a percent-encoded URL. Bad percent encoding must receive a client error, not a server crash. Keep HTML/text content types and the existing index route. Add visible tests.

Not done: decoding only the one example filename or weakening existing MIME/status behavior.
