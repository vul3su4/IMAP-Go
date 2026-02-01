package imapgo

type ImapOpts struct {
	Imap             *EmailOpts
	Site             string
	ReceiverEmail    string
	ReceiverPassword string
	CatchallEmail    string
	CatchallPassword string
	MaxChecks        int
	TimeRangeMinutes int // only parse emails in the last N minutes (0=no limit)
	TargetFetchCount int // Target: fetch the last N emails from each mailbox (0=default 100)
}

type EmailOpts struct {
	Email string
	Imap  string
}

// TargetOrderInfo from Target order confirmation email
type TargetOrderInfo struct {
	OrderNumber   string // Order number
	OrderTotal    string // Order total (e.g. $121.58)
	DeliversTo    string // Delivery address
	ProductName   string // Product name
	ProductImage  string // Product image URL (for UI, e.g. target.scene7.com/...)
	ReceivedAt    int64  // Received time (Unix timestamp)
}

// TargetOrderStatus 查詢特定訂單的狀態與追蹤號
// Status 優先級: canceled > arrived > ready to ship > confirmed > unknown
type TargetOrderStatus struct {
	OrderNumber    string // 訂單號碼
	Status         string // "confirmed" | "canceled" | "ready to ship" | "arrived" | "unknown"
	ReceivedAt     int64  // 最新相關信的收件時間 (Unix timestamp)
	Subject        string // 最新相關信主旨（可選）
	TrackingNumber string // 追蹤號（來自 ready to ship / arrived 信，可選）
}

type AmazonOrderInfo struct {
	OrderNumber   string // Order number
	OrderTotal    string // Order total (e.g. $121.58)
	DeliversTo    string // Delivery address
	ProductName   string // Product name
	ProductImage  string // Product image URL (for UI, e.g. target.scene7.com/...)
	ReceivedAt    int64  // Received time (Unix timestamp)
}

type AmazonOrderStatus struct {
	OrderNumber    string // Order number
	Status         string // "confirmed" | "canceled" | "ready to ship" | "arrived" | "unknown"
	ReceivedAt     int64  // Received time (Unix timestamp) of the latest related email
	Subject        string // Subject of the latest related email (optional)
	TrackingNumber string // Tracking number (from ready to ship / arrived email, optional)
}

type BestBuyOrderInfo struct {
	OrderNumber   string // Order number
	OrderTotal    string // Order total (e.g. $121.58)
	DeliversTo    string // Delivery address
	ProductName   string // Product name
	ProductImage  string // Product image URL (for UI, e.g. target.scene7.com/...)
	ReceivedAt    int64  // Received time (Unix timestamp)
}

type BestBuyOrderStatus struct {
	OrderNumber    string // Order number
	Status         string // "confirmed" | "canceled" | "ready to ship" | "arrived" | "unknown"
	ReceivedAt     int64  // Received time (Unix timestamp) of the latest related email
	Subject        string // Subject of the latest related email (optional)
	TrackingNumber string // Tracking number (from ready to ship / arrived email, optional)
}

type WalmartOrderInfo struct {
	OrderNumber   string // Order number
	OrderTotal    string // Order total (e.g. $121.58)
	DeliversTo    string // Delivery address
	ProductName   string // Product name
	ProductImage  string // Product image URL (for UI, e.g. target.scene7.com/...)
	ReceivedAt    int64  // Received time (Unix timestamp)
}

type WalmartOrderStatus struct {
	OrderNumber    string // Order number
	Status         string // "confirmed" | "canceled" | "ready to ship" | "arrived" | "unknown"
	ReceivedAt     int64  // Received time (Unix timestamp) of the latest related email
	Subject        string // Subject of the latest related email (optional)
	TrackingNumber string // Tracking number (from ready to ship / arrived email, optional)
}

