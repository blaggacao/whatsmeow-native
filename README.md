# whatsmeow-native

[whatsmeow](https://github.com/tulir/whatsmeow) is a Go library for the WhatsApp web multidevice API.
This project contains whatsmeow-native which aims to be a wrapper around whatsmeow, exposing a native
interface which may be used in other programming languages such as Android's Java and Kotlin.
For the time being, though, whatsmeow-native isn't used as a library but instead runs as a binary.
Android applications can interact with it by reading the output of the process and send text messages
into its input.

This project is initially based on whatsmeow's `mdtest` example binary.

## Limitations

whatsmeow-native isn't intended to be used as a fully-featured WhatsApp client. Instead, it tries to implement
the most important basic functions so that the WhatsApp infrastructure can be used as a transport layer in
other applications. This means that the following functions are implemented:

* Pair a phone via mobile number and resulting pair code
* Get a list of WhatsApp contacts
* Send files to WhatsApp contacts
* Receive files from WhatsApp contacts

Everything else is not considered necessary for the mentioned use case and is therefore not implemented.

## Usage

The code inside this repository can be built by calling

```
go build
```

Afterwards, the resulting `whatsmeow-native` binary can be executed. The available commands to control the
binary may be found in the `handleCmd` function of the _main.go_ file. You may want to start the binary with
the `-device-name` flag to customize the device name shown inside WhatsApp.

Here is an excerpt of available commands:

### Pair phone

`pair-phone <number>`

Returns: An 8-character string, separated by a dash.

Example: `AB2C-3Yh4`

### List contacts

`list-contacts`

Returns: A JSON dictionary with the contacts' phone numbers as keys and a `ContactInfo` object as value.

Example:

```json
{
    "491771234567@s.whatsapp.net": {
        "Found":true,
        "FirstName":"Alice",
        "FullName":"Alice Mayer",
        "PushName":"Alice",
        "BusinessName":""
    },
    "491777654321@s.whatsapp.net": {
        "Found":true,
        "FirstName":"Bob",
        "FullName":"Bob Juan",
        "PushName":"Bob",
        "BusinessName":""
    }
}
```

### Send image

`send-img <jid> <image path> [caption]`

### Receive image


