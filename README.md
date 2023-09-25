# whatsmeow-native

[whatsmeow](https://github.com/tulir/whatsmeow) is a Go library for the WhatsApp web multidevice API.
This project contains whatsmeow-native which is a wrapper around whatsmeow, exposing a native
interface which may be used in other programming languages such as Android's Java and Kotlin.

This project is initially based on whatsmeow's `mdtest` example binary.

## Usage

1. Clone the repository.
2. Run `go build` inside this directory.
3. Run `./mdtest` to start the program. Optionally, use `rlwrap ./mdtest` to get a nicer prompt.
   Add `-debug` if you want to see the raw data being sent/received.
4. On the first run, scan the QR code. On future runs, the program will remember you (unless `mdtest.db` is deleted). 

New messages will be automatically logged. To send a message, use `send <jid> <message>`
