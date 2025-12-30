# IMAP-Go

A Go library for fetching email verification codes from IMAP servers.
This supports  (Gmail, Outlook) and can extract verification codes from  websites (Nike, Cityline).

## Installation

```bash
go get github.com/vul3su4/IMAP-Go
```

## Features

-  Support for Gmail and Outlook IMAP servers
-  Automatic email verification code extraction
-  Support for Nike and Cityline websites
-  Retry mechanism with configurable max checks
-  Catchall email support
-  Automatic email parsing and code extraction

## Usage

### Basic Example

```go
package main

import (
	"log"
	imapgoprebuilt "github.com/vul3su4/IMAP-Go"
)

func main() {
	imapOpts := &imapgoprebuilt.ImapOpts{
		Imap:            imapgoprebuilt.Gmail,
		Site:            imapgoprebuilt.Nike,
		ReceiverEmail:   "EMAIL@gmail.com",
		ReceiverPassword: "APP_PASSWORD",
		CatchallEmail:   "",
		CatchallPassword: "",
		MaxChecks:       5,
	}

	if code, err := imapOpts.FetchEmail(); err != nil {
		log.Println(err)
	} else {
		log.Println(code)
	}
}
```

## Configuration

### ImapOpts Structure

```go
type ImapOpts struct {
	Imap            *EmailOpts  // IMAP server configuration (Gmail or Outlook)
	Site            string       // Target site ("nike" or "cityline")
	ReceiverEmail   string       // Email address to receive codes
	ReceiverPassword string      // App password for the email account
	CatchallEmail   string       // Optional: Catchall email address
	CatchallPassword string      // Optional: Catchall email password
	MaxChecks       int          // Maximum number of retry attempts
}
```

### Supported IMAP Providers

- `imapgoprebuilt.Gmail` - Gmail IMAP server (imap.gmail.com:993)
- `imapgoprebuilt.Outlook` - Outlook IMAP server (imap.outlook.com:993)

### Supported Sites

- `imapgoprebuilt.Nike` - Nike verification codes
- `imapgoprebuilt.Cityline` - Cityline verification codes

## Examples

### Using Gmail with Nike

```go
imapOpts := &imapgoprebuilt.ImapOpts{
	Imap:            imapgoprebuilt.Gmail,
	Site:            imapgoprebuilt.Nike,
	ReceiverEmail:   "your.email@gmail.com",
	ReceiverPassword: "your-app-password",
	MaxChecks:       5,
}

code, err := imapOpts.FetchEmail()
if err != nil {
	log.Fatal(err)
}
fmt.Println("Verification code:", code)
```

### Using Outlook with Cityline

```go
imapOpts := &imapgoprebuilt.ImapOpts{
	Imap:            imapgoprebuilt.Outlook,
	Site:            imapgoprebuilt.Cityline,
	ReceiverEmail:   "your.email@outlook.com",
	ReceiverPassword: "your-app-password",
	MaxChecks:       10,
}

code, err := imapOpts.FetchEmail()
if err != nil {
	log.Fatal(err)
}
fmt.Println("Verification code:", code)
```

### Using Catchall Email

```go
imapOpts := &imapgoprebuilt.ImapOpts{
	Imap:            imapgoprebuilt.Gmail,
	Site:            imapgoprebuilt.Nike,
	ReceiverEmail:   "main@gmail.com",
	ReceiverPassword: "main-password",
	CatchallEmail:   "catchall@example.com",
	CatchallPassword: "catchall-password",
	MaxChecks:       5,
}

code, err := imapOpts.FetchEmail()
```

## How It Works

1. Connects to the specified IMAP server using TLS
2. Authenticates with the provided email credentials
3. Searches through email mailboxes for verification emails
4. Extracts verification codes using site-specific parsing logic
5. Retries up to `MaxChecks` times with 5-second intervals if no code is found
6. Returns the extracted verification code

## Requirements

- Valid email account with IMAP access enabled
- App password (for Gmail, you need to generate an app-specific password)

## Gmail Setup

To use Gmail with this library:

1. Enable 2-Step Verification on your Google account
2. Generate an App Password:
   - Go to your Google Account settings
   - Security → 2-Step Verification → App passwords
   - Generate a new app password for "Mail"
   - Use this password as `ReceiverPassword`


```go
code, err := imapOpts.FetchEmail()
if err != nil {
	log.Printf("Failed to fetch email: %v", err)
	return
}
```


