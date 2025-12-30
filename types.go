package imapgo

type ImapOpts struct{
	Imap *EmailOpts
	Site string
	ReceiverEmail string
	ReceiverPassword string
	CatchallEmail string
	CatchallPassword string
	MaxChecks int
}

type EmailOpts struct{
	Email string
	Imap string
}