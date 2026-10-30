# AI usage

**Tool:** Claude (Anthropic), used through a chat interface with a code sandbox that could build and run Go.

## What the AI did

I gave Claude the assessment text and asked it to build the whole project. It:

- chose the design (Go, SQLite, REST; pure rules package; one transaction per batch; rules also enforced by database triggers and constraints);
- wrote the application code, the fictional supplier server, and the fictional supplier data in `data/feeds/`;
- wrote the automated tests, the demo script (`scripts/demo.sh`), the Postman collection, `README.md` and `DEMO.md`;
- ran `gofmt`, `go vet`, `go test -race` and the demo in its sandbox, and fixed problems it found.

I used the same chat to help me get it running on my own Windows machine: the C compiler (CGO) setup for the SQLite driver, and Windows-specific problems in the demo script.

## Where the AI was wrong, and what testing found

- Running the demo showed that importing the same feed twice created duplicate open issues (10 instead of 5). The design was changed to de-duplicate open issues.
- The demo script's crash step silently failed on Windows (the old server was never stopped). The script was changed to stop processes by port and to fail loudly if the crash does not happen.
- A port-check in the script gave false "port in use" errors on my machine and was corrected.

The tests were run by the AI in its Linux sandbox. I ran the demo on Windows myself. The test suite and the code have only been executed on Linux and Windows (Git Bash) as described in the README.
