package imapgo

type ImapOpts struct {
	Imap             *EmailOpts
	Site             string
	ReceiverEmail    string
	ReceiverPassword string
	CatchallEmail    string
	CatchallPassword string
	MaxChecks        int
	TimeRangeMinutes int // Time range in minutes to search for emails (0 = no limit, default: 10)
}

type EmailOpts struct {
	Email string
	Imap  string
}
