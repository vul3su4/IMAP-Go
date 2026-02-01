package imapgo

import (
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/client"
	_ "github.com/emersion/go-message/charset"
	"github.com/emersion/go-message/mail"
)

// GetTargetOrderList from last 1000 emails (default) find target order info
func (n *ImapOpts) GetTargetOrderList() ([]*TargetOrderInfo, error) {
	c, err := client.DialTLS(n.Imap.Imap, nil)
	if err != nil {
		return nil, errors.New("could not connect to mail server")
	}
	defer c.Logout()

	if n.CatchallPassword == "" {
		if err := c.Login(n.ReceiverEmail, n.ReceiverPassword); err != nil {
			return nil, fmt.Errorf("login and password are incorrect: %s:%s - %s", n.ReceiverEmail, n.ReceiverPassword, err.Error())
		}
	} else {
		if err := c.Login(n.CatchallEmail, n.CatchallPassword); err != nil {
			return nil, fmt.Errorf("login and password are incorrect: %s:%s - %s", n.CatchallEmail, n.CatchallPassword, err.Error())
		}
	}

	var boxes []string
	mailboxes := make(chan *imap.MailboxInfo, 5)
	done := make(chan error, 1)
	go func() {
		done <- c.List("", "*", mailboxes)
	}()

	for m := range mailboxes {
		if m.Name == "[Gmail]/Important" {
			continue
		}
		boxes = append(boxes, m.Name)
	}

	if err := <-done; err != nil {
		return nil, fmt.Errorf("login and password are incorrect: %s:%s - %s", n.CatchallEmail, n.CatchallPassword, err.Error())
	}

	var list []*TargetOrderInfo
	fetchCount := n.TargetFetchCount
	if fetchCount <= 0 {
		fetchCount = 50
	}

	for _, box := range boxes {
		mbox, err := c.Select(box, false)
		if err != nil {
			continue
		}
		if mbox.Messages == 0 {
			continue
		}

		var from, to uint32
		if mbox.Messages > uint32(fetchCount) {
			from = mbox.Messages
			to = mbox.Messages - uint32(fetchCount)
		} else {
			from = mbox.Messages
			to = 0
		}

		seqSet := new(imap.SeqSet)
		seqSet.AddRange(from, to)

		var section imap.BodySectionName
		items := []imap.FetchItem{section.FetchItem()}
		messages := make(chan *imap.Message, 8)
		go func() {
			c.Fetch(seqSet, items, messages)
		}()

		for msg := range messages {
			if msg == nil {
				continue
			}
			r := msg.GetBody(&section)
			if r == nil {
				continue
			}

			mr, err := mail.CreateReader(r)
			if err != nil {
				continue
			}

			header := mr.Header

			var maildate int64
			if date, err := header.Date(); err == nil {
				maildate = date.Unix()
			}

			if n.TimeRangeMinutes > 0 {
				if maildate < time.Now().Unix()-int64(n.TimeRangeMinutes)*60 {
					continue
				}
			}

			mailsubject, _ := header.Subject()
			mailsubject = strings.ToLower(mailsubject)
			if !strings.Contains(mailsubject, "order") && !strings.Contains(mailsubject, "thanks for shopping") {
				continue
			}

			var fromaddress string
			if fromList, err := header.AddressList("From"); err == nil && len(fromList) > 0 {
				fromaddress = fromList[0].String()
			}
			fromLower := strings.ToLower(fromaddress)
			if !strings.Contains(fromLower, "target.com") {
				continue
			}

			// Recipient selection: support direct delivery, forwarding, catchall
			// - When using catchall login: all emails in the mailbox are considered (no To filter)
			// - Otherwise: To or Delivered-To (forwarded) one of the ReceiverEmail matches; Gmail user+label is considered the same mailbox
			if n.CatchallPassword == "" {
				var toAddress, deliveredTo string
				if toList, err := header.AddressList("To"); err == nil && len(toList) > 0 {
					toAddress = toList[0].String()
				}
				if h := header.Get("Delivered-To"); h != "" {
					deliveredTo = strings.TrimSpace(h)
				}
				matchesTo := targetAddressMatches(toAddress, n.ReceiverEmail)
				matchesDelivered := deliveredTo != "" && targetAddressMatches(deliveredTo, n.ReceiverEmail)
				if !matchesTo && !matchesDelivered {
					continue
				}
			}

			var htmlBody string
			for {
				p, err := mr.NextPart()
				if err == io.EOF {
					break
				}
				if err != nil {
					break
				}
				if _, ok := p.Header.(*mail.InlineHeader); ok {
					b, _ := io.ReadAll(p.Body)
					body := string(b)
					if body == "" {
						continue
					}
					bodyLower := strings.ToLower(body)
					hasTags := strings.Contains(body, "<") && strings.Contains(body, ">")
					if hasTags && (strings.Contains(bodyLower, "order-total") ||
						strings.Contains(bodyLower, "qty:") ||
						strings.Contains(bodyLower, "delivers to") ||
						strings.Contains(bodyLower, "$") && strings.Contains(bodyLower, "</h3>")) {
						htmlBody = body
						break
					}
					if hasTags && htmlBody == "" {
						htmlBody = body
					}
					if htmlBody == "" {
						htmlBody = body
					}
				}
			}

			if htmlBody == "" {
				continue
			}

			info := parseTargetOrderFromHTML(htmlBody)
			if info.OrderNumber == "" {
				info.OrderNumber = parseOrderNumberFromSubject(mailsubject)
			}
			if info.OrderNumber == "" {
				continue
			}
			if isTargetCancellationEmail(htmlBody) {
				continue
			}
			info.ReceivedAt = maildate
			list = append(list, info)
		}
	}

	// Remove duplicates: the same email may appear multiple times in Gmail labels (INBOX, Promotions, etc.), only keep the first occurrence by OrderNumber
	list = dedupeTargetOrdersByOrderNumber(list)
	return list, nil
}

// dedupeTargetOrdersByOrderNumber remove duplicates by OrderNumber (keep the first occurrence of each order)
func dedupeTargetOrdersByOrderNumber(list []*TargetOrderInfo) []*TargetOrderInfo {
	seen := make(map[string]bool)
	var out []*TargetOrderInfo
	for _, info := range list {
		if info == nil || info.OrderNumber == "" {
			continue
		}
		if seen[info.OrderNumber] {
			continue
		}
		seen[info.OrderNumber] = true
		out = append(out, info)
	}
	return out
}

// GetTargetOrderInfo return the first Target order confirmation email (compatible with old call)
func (n *ImapOpts) GetTargetOrderInfo() (*TargetOrderInfo, error) {
	list, err := n.GetTargetOrderList()
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, errors.New("no order found")
	}
	return list[0], nil
}

// GetTargetOrderStatus check if the order is canceled or updated (scan the last N Target emails)
// Return Status: "confirmed" | "canceled" | "ready to ship" | "arrived" | "unknown"; based on the latest related email
func (n *ImapOpts) GetTargetOrderStatus(orderNumber string) (*TargetOrderStatus, error) {
	if orderNumber == "" {
		return &TargetOrderStatus{OrderNumber: "", Status: "unknown", ReceivedAt: 0}, nil
	}
	orderNumber = strings.TrimSpace(orderNumber)

	c, err := client.DialTLS(n.Imap.Imap, nil)
	if err != nil {
		return nil, errors.New("could not connect to mail server")
	}
	defer c.Logout()

	if n.CatchallPassword == "" {
		if err := c.Login(n.ReceiverEmail, n.ReceiverPassword); err != nil {
			return nil, fmt.Errorf("login and password are incorrect: %s:%s - %s", n.ReceiverEmail, n.ReceiverPassword, err.Error())
		}
	} else {
		if err := c.Login(n.CatchallEmail, n.CatchallPassword); err != nil {
			return nil, fmt.Errorf("login and password are incorrect: %s:%s - %s", n.CatchallEmail, n.CatchallPassword, err.Error())
		}
	}

	var boxes []string
	mailboxes := make(chan *imap.MailboxInfo, 5)
	done := make(chan error, 1)
	go func() {
		done <- c.List("", "*", mailboxes)
	}()
	for m := range mailboxes {
		if m.Name == "[Gmail]/Important" {
			continue
		}
		boxes = append(boxes, m.Name)
	}
	if err := <-done; err != nil {
		return nil, fmt.Errorf("login and password are incorrect: %s:%s - %s", n.CatchallEmail, n.CatchallPassword, err.Error())
	}

	fetchCount := n.TargetFetchCount
	if fetchCount <= 0 {
		fetchCount = 100
	}

	// search for order status only in INBOX and [Gmail]/All Mail, avoid scanning 20+ folders to avoid performance issues
	statusBoxes := statusCheckMailboxes(boxes)

	// scan Target emails, only keep emails with the order number in the subject or body; priority: canceled > arrived > ready to ship > confirmed
	var latestConfirmedAt int64
	var latestConfirmedSubject string
	var latestCanceledAt int64
	var latestCanceledSubject string
	var latestReadyToShipAt int64
	var latestReadyToShipSubject string
	var latestReadyToShipTracking string
	var latestArrivedAt int64
	var latestArrivedSubject string
	var latestArrivedTracking string

	for _, box := range statusBoxes {
		mbox, err := c.Select(box, false)
		if err != nil || mbox.Messages == 0 {
			continue
		}
		var from, to uint32
		if mbox.Messages > uint32(fetchCount) {
			from = mbox.Messages
			to = mbox.Messages - uint32(fetchCount)
		} else {
			from = mbox.Messages
			to = 0
		}
		seqSet := new(imap.SeqSet)
		seqSet.AddRange(from, to)
		var section imap.BodySectionName
		items := []imap.FetchItem{section.FetchItem()}
		messages := make(chan *imap.Message, 8)
		go func() {
			c.Fetch(seqSet, items, messages)
		}()

		for msg := range messages {
			if msg == nil {
				continue
			}
			r := msg.GetBody(&section)
			if r == nil {
				continue
			}
			mr, err := mail.CreateReader(r)
			if err != nil {
				continue
			}
			header := mr.Header
			var maildate int64
			if date, err := header.Date(); err == nil {
				maildate = date.Unix()
			}
			if n.TimeRangeMinutes > 0 && maildate < time.Now().Unix()-int64(n.TimeRangeMinutes)*60 {
				continue
			}
			mailsubject, _ := header.Subject()
			mailsubjectLower := strings.ToLower(mailsubject)
			if !strings.Contains(mailsubjectLower, "order") && !strings.Contains(mailsubjectLower, "thanks for shopping") && !strings.Contains(mailsubjectLower, "cancel") &&
				!strings.Contains(mailsubjectLower, "ship") && !strings.Contains(mailsubjectLower, "arrived") {
				continue
			}
			var fromaddress string
			if fromList, err := header.AddressList("From"); err == nil && len(fromList) > 0 {
				fromaddress = fromList[0].String()
			}
			if !strings.Contains(strings.ToLower(fromaddress), "target.com") {
				continue
			}
			if n.CatchallPassword == "" {
				var toAddress, deliveredTo string
				if toList, err := header.AddressList("To"); err == nil && len(toList) > 0 {
					toAddress = toList[0].String()
				}
				if h := header.Get("Delivered-To"); h != "" {
					deliveredTo = strings.TrimSpace(h)
				}
				matchesTo := targetAddressMatches(toAddress, n.ReceiverEmail)
				matchesDelivered := deliveredTo != "" && targetAddressMatches(deliveredTo, n.ReceiverEmail)
				if !matchesTo && !matchesDelivered {
					continue
				}
			}

			var htmlBody string
			for {
				p, err := mr.NextPart()
				if err == io.EOF {
					break
				}
				if err != nil {
					break
				}
				if _, ok := p.Header.(*mail.InlineHeader); ok {
					b, _ := io.ReadAll(p.Body)
					htmlBody = string(b)
					break
				}
			}
			if htmlBody == "" {
				continue
			}

			// only process emails with the order number in the subject or body
			if !strings.Contains(mailsubject, orderNumber) && !strings.Contains(htmlBody, orderNumber) {
				continue
			}

			isCanceled := isTargetCancellationEmail(htmlBody) || isTargetCancellationSubject(mailsubject)
			isReadyToShip := isTargetReadyToShipEmail(htmlBody, mailsubject)
			isArrived := isTargetArrivedEmail(htmlBody, mailsubject)

			if isCanceled {
				if maildate > latestCanceledAt {
					latestCanceledAt = maildate
					latestCanceledSubject = mailsubject
				}
			} else if isArrived {
				if maildate > latestArrivedAt {
					latestArrivedAt = maildate
					latestArrivedSubject = mailsubject
					latestArrivedTracking = parseTargetTrackingNumber(htmlBody)
				}
			} else if isReadyToShip {
				if maildate > latestReadyToShipAt {
					latestReadyToShipAt = maildate
					latestReadyToShipSubject = mailsubject
					latestReadyToShipTracking = parseTargetTrackingNumber(htmlBody)
				}
			} else {
				if maildate > latestConfirmedAt {
					latestConfirmedAt = maildate
					latestConfirmedSubject = mailsubject
				}
			}
		}
	}

	res := &TargetOrderStatus{
		OrderNumber:    orderNumber,
		Status:         "unknown",
		ReceivedAt:     0,
		Subject:        "",
		TrackingNumber: "",
	}
	// priority: canceled > arrived > ready to ship > confirmed
	if latestCanceledAt > 0 {
		res.Status = "canceled"
		res.ReceivedAt = latestCanceledAt
		res.Subject = latestCanceledSubject
	} else if latestArrivedAt > 0 {
		res.Status = "arrived"
		res.ReceivedAt = latestArrivedAt
		res.Subject = latestArrivedSubject
		res.TrackingNumber = latestArrivedTracking
	} else if latestReadyToShipAt > 0 {
		res.Status = "ready to ship"
		res.ReceivedAt = latestReadyToShipAt
		res.Subject = latestReadyToShipSubject
		res.TrackingNumber = latestReadyToShipTracking
	} else if latestConfirmedAt > 0 {
		res.Status = "confirmed"
		res.ReceivedAt = latestConfirmedAt
		res.Subject = latestConfirmedSubject
	}
	return res, nil
}

// targetAddressMatches check if the recipient is the same mailbox (supports Gmail user+label@gmail.com)
func targetAddressMatches(toAddress, receiverEmail string) bool {
	normTo := normalizeEmailForMatch(toAddress)
	normRecv := normalizeEmailForMatch(receiverEmail)
	return normTo != "" && normRecv != "" && normTo == normRecv
}

func normalizeEmailForMatch(addr string) string {
	addr = strings.TrimSpace(strings.ToLower(addr))
	// extract the email address from <...>
	if start := strings.Index(addr, "<"); start >= 0 {
		if end := strings.Index(addr, ">"); end > start {
			addr = addr[start+1 : end]
		}
	}
	at := strings.LastIndex(addr, "@")
	if at <= 0 {
		return addr
	}
	local := addr[:at]
	domain := addr[at:]
	// Gmail: user+label@gmail.com and user@gmail.com are considered the same mailbox
	if plus := strings.Index(local, "+"); plus > 0 {
		local = local[:plus]
	}
	return local + domain
}

// isTargetCancellationEmail check if it is a "order canceled" email (common in body: Sorry we had to cancel, Your order has been canceled, etc.)
func isTargetCancellationEmail(html string) bool {
	lower := strings.ToLower(html)
	return strings.Contains(lower, "order has been canceled") ||
		strings.Contains(lower, "order has been cancelled") ||
		strings.Contains(lower, "your order has<br>canceled") ||
		strings.Contains(lower, "what went wrong?") ||
		strings.Contains(lower, "was canceled") ||
		strings.Contains(lower, "had to cancel") ||
		(strings.Contains(lower, "sorry") && strings.Contains(lower, "cancel"))
}

// statusCheckMailboxes search for order status only in INBOX and [Gmail]/All Mail, avoid scanning all folders to avoid performance issues
func statusCheckMailboxes(boxes []string) []string {
	var out []string
	for _, b := range boxes {
		if b == "INBOX" || b == "[Gmail]/All Mail" {
			out = append(out, b)
		}
	}
	if len(out) == 0 {
		return boxes // fallback: no INBOX/All Mail, use all folders
	}
	return out
}

// isTargetCancellationSubject 主旨是否為取消信（例：Sorry, we had to cancel order #...）
func isTargetCancellationSubject(subject string) bool {
	lower := strings.ToLower(subject)
	return strings.Contains(lower, "had to cancel") ||
		(strings.Contains(lower, "sorry") && strings.Contains(lower, "cancel"))
}

// isTargetReadyToShipEmail check if it is a "ready to ship" email (example: Get ready for something special! Items from order #... are about to ship.)
func isTargetReadyToShipEmail(html, subject string) bool {
	lowerH := strings.ToLower(html)
	lowerS := strings.ToLower(subject)
	return strings.Contains(lowerH, "about to ship") ||
		strings.Contains(lowerS, "about to ship") ||
		(strings.Contains(lowerH, "ready") && strings.Contains(lowerH, "ship")) ||
		(strings.Contains(lowerS, "ready") && strings.Contains(lowerS, "ship"))
}

// isTargetArrivedEmail check if it is a "arrived" email (example: Items have arrived from order #...!)
func isTargetArrivedEmail(html, subject string) bool {
	lowerH := strings.ToLower(html)
	lowerS := strings.ToLower(subject)
	return strings.Contains(lowerH, "have arrived") ||
		strings.Contains(lowerS, "have arrived") ||
		strings.Contains(lowerH, "items have arrived") ||
		strings.Contains(lowerS, "items have arrived")
}

// parseTargetTrackingNumber 從內文抓追蹤號（例：United Parcel Service Tracking # 1ZWY0657YW48341703）
func parseTargetTrackingNumber(html string) string {
	// Tracking # 1ZWY0657YW48341703 或 United Parcel Service Tracking # ...
	re := regexp.MustCompile(`(?i)(?:united\s+parcel\s+service\s+)?tracking\s*#\s*([A-Z0-9]{10,30})`)
	if m := re.FindStringSubmatch(html); len(m) > 1 {
		return strings.TrimSpace(m[1])
	}
	re2 := regexp.MustCompile(`(?i)tracking\s*#\s*([A-Z0-9]{10,30})`)
	if m := re2.FindStringSubmatch(html); len(m) > 1 {
		return strings.TrimSpace(m[1])
	}
	return ""
}

// looksLikeAddress exclude buttons/links, only keep content that looks like an address
func looksLikeAddress(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" || len(s) < 10 {
		return false
	}
	lower := strings.ToLower(s)
	// exclude common non-address content
	for _, bad := range []string{"what went wrong", "-->", "visit ", "view order", "view item", "track package"} {
		if strings.Contains(lower, bad) {
			return false
		}
	}
	// Address usually has a comma or number (door number/postal code)
	hasComma := strings.Contains(s, ",")
	hasDigit := regexp.MustCompile(`\d`).MatchString(s)
	return hasComma || (hasDigit && len(s) > 15)
}

func parseOrderNumberFromSubject(subject string) string {
	re := regexp.MustCompile(`(?i)order\s*#\s*:?\s*(\d+)`)
	m := re.FindStringSubmatch(subject)
	if len(m) > 1 {
		return strings.TrimSpace(m[1])
	}
	return ""
}

func parseTargetOrderFromHTML(html string) *TargetOrderInfo {
	info := &TargetOrderInfo{}

	// Order number (body) e.g. Order #102XXXXXXXor # 1XXXXXXXX
	orderNumRe := regexp.MustCompile(`(?i)order\s*#\s*:?\s*(\d+)`)
	if m := orderNumRe.FindStringSubmatch(html); len(m) > 1 {
		info.OrderNumber = strings.TrimSpace(m[1])
	}

	// Order total (consumption): Target structure may be class="...order-total-price">$121.58</h3> or Gmail has changed
	orderTotalRe := regexp.MustCompile(`order-total-price">\$([\d,]+\.[0-9]{2})</h3>`)
	if m := orderTotalRe.FindStringSubmatch(html); len(m) > 1 {
		info.OrderTotal = "$" + strings.TrimSpace(m[1])
	}
	if info.OrderTotal == "" {
		// Backup: any <h3> inside only has the amount
		orderTotalRe2 := regexp.MustCompile(`<h3[^>]*>\s*\$([\d,]+\.[0-9]{2})\s*</h3>`)
		if m := orderTotalRe2.FindStringSubmatch(html); len(m) > 1 {
			info.OrderTotal = "$" + strings.TrimSpace(m[1])
		}
	}
	if info.OrderTotal == "" {
		orderTotalRe3 := regexp.MustCompile(`(?is)(?:order\s+)?total[\s\S]{0,800}?\$([\d,]+\.[0-9]{2})`)
		if m := orderTotalRe3.FindStringSubmatch(html); len(m) > 1 {
			info.OrderTotal = "$" + strings.TrimSpace(m[1])
		}
	}
	if info.OrderTotal == "" {
		orderTotalRe4 := regexp.MustCompile(`\$([\d,]+\.[0-9]{2})`)
		if all := orderTotalRe4.FindAllStringSubmatch(html, -1); len(all) > 0 {
			info.OrderTotal = "$" + strings.TrimSpace(all[len(all)-1][1])
		}
	}

	// Delivers to / address: Target is <span>XXXXXX, XXXXX Ave ...</span>
	deliversRe := regexp.MustCompile(`(?is)(?:delivers\s+to|shipping)\s*:?[\s\S]*?<span[^>]*>([^<]+)</span>`)
	if m := deliversRe.FindStringSubmatch(html); len(m) > 1 {
		info.DeliversTo = strings.TrimSpace(m[1])
		info.DeliversTo = regexp.MustCompile(`\s+`).ReplaceAllString(info.DeliversTo, " ")
	}
	if info.DeliversTo == "" {
		// Backup: any span content contains a comma and looks like an address (20+ characters, has numbers)
		deliversRe2 := regexp.MustCompile(`<span[^>]*>([^<]{20,200})</span>`)
		for _, m := range deliversRe2.FindAllStringSubmatch(html, -1) {
			c := strings.TrimSpace(m[1])
			if strings.Contains(c, ",") && regexp.MustCompile(`\d`).MatchString(c) && looksLikeAddress(c) {
				info.DeliversTo = regexp.MustCompile(`\s+`).ReplaceAllString(c, " ")
				break
			}
		}
	}
	if info.DeliversTo == "" {
		deliversRe3 := regexp.MustCompile(`(?is)delivers\s+to\s*:?\s*([^<\n]+)`)
		if m := deliversRe3.FindStringSubmatch(html); len(m) > 1 {
			info.DeliversTo = strings.TrimSpace(m[1])
			info.DeliversTo = regexp.MustCompile(`\s+`).ReplaceAllString(info.DeliversTo, " ")
		}
	}
	if !looksLikeAddress(info.DeliversTo) {
		info.DeliversTo = ""
	}

	// Product name：<a href="...target...">2025 POK ME 2.5 Elite Trainer Box</a> ... Qty: 2
	productRe := regexp.MustCompile(`(?is)<a[^>]+href="[^"]*target[^"]*"[^>]*>([^<]+)</a>[\s\S]*?Qty:\s*\d`)
	if m := productRe.FindStringSubmatch(html); len(m) > 1 {
		info.ProductName = strings.TrimSpace(m[1])
	}
	if info.ProductName == "" {
		productRe2 := regexp.MustCompile(`(?is)<a[^>]+>([^<]{8,150})</a>[\s\S]*?Qty:\s*\d`)
		if m := productRe2.FindStringSubmatch(html); len(m) > 1 {
			info.ProductName = strings.TrimSpace(m[1])
		}
	}
	if info.ProductName == "" {
		// Backup: any >text< followed by Qty: (single quote attribute, extra whitespace)
		productRe3 := regexp.MustCompile(`(?is)>\s*([^<]{10,120})\s*<[\s\S]*?[Qq]ty\s*:\s*\d`)
		if m := productRe3.FindStringSubmatch(html); len(m) > 1 {
			t := strings.TrimSpace(m[1])
			if !strings.HasPrefix(strings.ToLower(t), "view ") && !strings.HasPrefix(strings.ToLower(t), "track ") {
				info.ProductName = t
			}
		}
	}

	// Product image：<img src="...target.scene7.com/is/image/Target/GUEST_..." ... />（Gmail 可能包成 #https://target.scene7.com/...）
	info.ProductImage = parseTargetProductImageURL(html)

	return info
}

// parseTargetProductImageURL extract the product image URL from the body, return the original src (can be displayed directly in the web <img> when using Gmail proxy)
// exclude logo/icon (e.g. bullseye_email, down-arrow, icon-info_on), prioritize product alt or GUEST_/gray-bag product images
func parseTargetProductImageURL(html string) string {
	// known non-product image path keywords (common logo/icon in Target emails)
	skipSlugs := map[string]bool{
		"bullseye_email": true, "bullseye": true, "down-arrow": true,
		"icon-info_on": true, "icon-info": true, "fulfillment-pattern": true,
	}
	// find all <img ... src="...target.scene7.com..." ...>, and optionally extract the alt tag inside the same tag
	reImg := regexp.MustCompile(`(?i)<img[^>]+src="([^"]*target\.scene7\.com[^"]*)"[^>]*(?:alt="([^"]*)"[^>]*)?>`)
	reAltFirst := regexp.MustCompile(`(?i)<img[^>]+alt="([^"]*)"[^>]+src="([^"]*target\.scene7\.com[^"]*)"[^>]*>`)
	matches := reImg.FindAllStringSubmatch(html, -1)
	if len(matches) == 0 {
		matches = reAltFirst.FindAllStringSubmatch(html, -1)
		if len(matches) > 0 {
			for i := range matches {
				matches[i][1], matches[i][2] = matches[i][2], matches[i][1] // 統一 [1]=src [2]=alt
			}
		}
	}
	if len(matches) == 0 {
		re2 := regexp.MustCompile(`(?i)src="([^"]*target\.scene7[^"]*)"`)
		if m := re2.FindStringSubmatch(html); len(m) > 1 {
			originalSrc := strings.TrimSpace(m[1])
			norm := normTargetScene7URL(originalSrc)
			if norm != "" && !isSkipSlug(norm, skipSlugs) {
				return originalSrc
			}
		}
		return ""
	}

	var bestOriginal string
	for _, m := range matches {
		if len(m) < 2 {
			continue
		}
		originalSrc := strings.TrimSpace(m[1])
		norm := normTargetScene7URL(originalSrc)
		if norm == "" {
			continue
		}
		slug := scene7Slug(norm)
		if skipSlugs[slug] {
			continue
		}
		alt := ""
		if len(m) > 2 {
			alt = strings.TrimSpace(m[2])
		}
		// 明顯產品圖：GUEST_、gray-bag 或 alt 像產品名（非 "Target Logo"/"arrow"/"Information Icon"）
		if strings.HasPrefix(slug, "GUEST_") || slug == "gray-bag" || (alt != "" && !isGenericAlt(alt)) {
			return originalSrc
		}
		if bestOriginal == "" {
			bestOriginal = originalSrc
		}
	}
	if bestOriginal != "" {
		return bestOriginal
	}
	return ""
}

func normTargetScene7URL(raw string) string {
	raw = strings.ReplaceAll(raw, "&amp;", "&")
	if idx := strings.Index(raw, "#https://target.scene7.com"); idx >= 0 {
		return strings.TrimSpace(raw[idx+1:])
	}
	if idx := strings.Index(raw, "#http"); idx >= 0 {
		return strings.TrimSpace(raw[idx+1:])
	}
	return raw
}

func scene7Slug(url string) string {
	// 從 .../Target/SLUG?... 取出 SLUG
	const prefix = "/Target/"
	if i := strings.Index(url, prefix); i >= 0 {
		rest := url[i+len(prefix):]
		if j := strings.IndexAny(rest, "?&#"); j >= 0 {
			return rest[:j]
		}
		return rest
	}
	return ""
}

func isSkipSlug(url string, skip map[string]bool) bool {
	return skip[scene7Slug(url)]
}

func isGenericAlt(alt string) bool {
	lower := strings.ToLower(alt)
	return lower == "target logo" || strings.Contains(lower, " arrow") ||
		strings.Contains(lower, "information icon") || len(alt) < 3
}
