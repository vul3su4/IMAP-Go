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

const amazonFetchCountDefault = 50
const amazonStatusFetchCountDefault = 100

// GetAmazonOrderList from last N emails find Amazon order info (confirmation emails)
func (n *ImapOpts) GetAmazonOrderList() ([]*AmazonOrderInfo, error) {
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
		return nil, fmt.Errorf("list mailboxes failed: %s", err.Error())
	}

	// Only scan INBOX and [Gmail]/All Mail (order emails land there); avoid 20+ folders for speed
	boxes = statusCheckMailboxes(boxes)

	var list []*AmazonOrderInfo
	fetchCount := amazonFetchCountDefault
	if n.TargetFetchCount > 0 {
		fetchCount = n.TargetFetchCount
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
			if !isAmazonOrderRelatedSubject(mailsubject) {
				continue
			}

			var fromaddress string
			if fromList, err := header.AddressList("From"); err == nil && len(fromList) > 0 {
				fromaddress = fromList[0].String()
			}
			fromLower := strings.ToLower(fromaddress)
			if !strings.Contains(fromLower, "amazon") {
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
					body := string(b)
					if body == "" {
						continue
					}
					bodyLower := strings.ToLower(body)
					hasTags := strings.Contains(body, "<") && strings.Contains(body, ">")
					if hasTags && (strings.Contains(bodyLower, "order") ||
						strings.Contains(bodyLower, "order total") ||
						strings.Contains(bodyLower, "ship to") ||
						strings.Contains(bodyLower, "amazon.com")) {
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

			if isAmazonCancellationEmail(htmlBody) {
				continue
			}

			info := parseAmazonOrderFromHTML(htmlBody)
			if info.OrderNumber == "" {
				info.OrderNumber = parseAmazonOrderNumberFromSubject(mailsubject)
			}
			if info.OrderNumber == "" {
				continue
			}
			info.ReceivedAt = maildate
			list = append(list, info)
		}
	}

	list = dedupeAmazonOrdersByOrderNumber(list)
	return list, nil
}

func dedupeAmazonOrdersByOrderNumber(list []*AmazonOrderInfo) []*AmazonOrderInfo {
	seen := make(map[string]bool)
	var out []*AmazonOrderInfo
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

// GetAmazonOrderInfo return the first Amazon order confirmation (compatible with FetchEmail usage)
func (n *ImapOpts) GetAmazonOrderInfo() (*AmazonOrderInfo, error) {
	list, err := n.GetAmazonOrderList()
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, errors.New("no order found")
	}
	return list[0], nil
}

// GetAmazonOrderStatus check order status (canceled / Shipped / Delivered / confirmed) from recent Amazon emails; subject like Shipped: "Product..." or Delivered: "Product..."
func (n *ImapOpts) GetAmazonOrderStatus(orderNumber string) (*AmazonOrderStatus, error) {
	orderNumber = strings.TrimSpace(orderNumber)
	if orderNumber == "" {
		return &AmazonOrderStatus{OrderNumber: "", Status: "unknown", ReceivedAt: 0}, nil
	}

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
		return nil, fmt.Errorf("list mailboxes failed: %s", err.Error())
	}

	statusBoxes := statusCheckMailboxes(boxes)
	fetchCount := amazonStatusFetchCountDefault
	if n.TargetFetchCount > 0 {
		fetchCount = n.TargetFetchCount
		if fetchCount < 50 {
			fetchCount = 100
		}
	}

	var latestConfirmedAt int64
	var latestConfirmedSubject string
	var latestCanceledAt int64
	var latestCanceledSubject string
	var latestShippedAt int64
	var latestShippedSubject string
	var latestShippedTracking string
	var latestDeliveredAt int64
	var latestDeliveredSubject string
	var latestDeliveredTracking string

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
			if !isAmazonOrderRelatedSubject(mailsubjectLower) {
				continue
			}
			var fromaddress string
			if fromList, err := header.AddressList("From"); err == nil && len(fromList) > 0 {
				fromaddress = fromList[0].String()
			}
			if !strings.Contains(strings.ToLower(fromaddress), "amazon") {
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

			if !strings.Contains(mailsubject, orderNumber) && !strings.Contains(htmlBody, orderNumber) {
				continue
			}

			isCanceled := isAmazonCancellationEmail(htmlBody) || isAmazonCancellationSubject(mailsubject)
			isDelivered := isAmazonDeliveredEmail(htmlBody, mailsubject)
			isShipped := isAmazonShippedEmail(htmlBody, mailsubject)

			if isCanceled {
				if maildate > latestCanceledAt {
					latestCanceledAt = maildate
					latestCanceledSubject = mailsubject
				}
			} else if isDelivered {
				if maildate > latestDeliveredAt {
					latestDeliveredAt = maildate
					latestDeliveredSubject = mailsubject
					latestDeliveredTracking = parseAmazonTrackingNumber(htmlBody)
				}
			} else if isShipped {
				if maildate > latestShippedAt {
					latestShippedAt = maildate
					latestShippedSubject = mailsubject
					latestShippedTracking = parseAmazonTrackingNumber(htmlBody)
				}
			} else {
				if maildate > latestConfirmedAt {
					latestConfirmedAt = maildate
					latestConfirmedSubject = mailsubject
				}
			}
		}
	}

	res := &AmazonOrderStatus{
		OrderNumber:    orderNumber,
		Status:         "unknown",
		ReceivedAt:     0,
		Subject:        "",
		TrackingNumber: "",
	}
	if latestCanceledAt > 0 {
		res.Status = "canceled"
		res.ReceivedAt = latestCanceledAt
		res.Subject = latestCanceledSubject
	} else if latestDeliveredAt > 0 {
		res.Status = "Delivered"
		res.ReceivedAt = latestDeliveredAt
		res.Subject = latestDeliveredSubject
		res.TrackingNumber = latestDeliveredTracking
	} else if latestShippedAt > 0 {
		res.Status = "Shipped"
		res.ReceivedAt = latestShippedAt
		res.Subject = latestShippedSubject
		res.TrackingNumber = latestShippedTracking
	} else if latestConfirmedAt > 0 {
		res.Status = "confirmed"
		res.ReceivedAt = latestConfirmedAt
		res.Subject = latestConfirmedSubject
	}
	return res, nil
}

func isAmazonOrderRelatedSubject(subject string) bool {
	lower := strings.ToLower(subject)
	return strings.Contains(lower, "ordered") ||
		strings.Contains(lower, "order") ||
		strings.Contains(lower, "shipped") ||
		strings.Contains(lower, "delivered") ||
		strings.Contains(lower, "cancel") ||
		strings.Contains(lower, "confirmation") ||
		strings.Contains(lower, "package") ||
		strings.Contains(lower, "shipment") ||
		strings.Contains(lower, "amazon")
}

func isAmazonCancellationEmail(html string) bool {
	lower := strings.ToLower(html)
	return strings.Contains(lower, "order has been canceled") ||
		strings.Contains(lower, "order has been cancelled") ||
		strings.Contains(lower, "we had to cancel") ||
		strings.Contains(lower, "had to cancel your order") ||
		(strings.Contains(lower, "sorry") && strings.Contains(lower, "cancel"))
}

func isAmazonCancellationSubject(subject string) bool {
	lower := strings.ToLower(subject)
	return strings.Contains(lower, "had to cancel") ||
		(strings.Contains(lower, "sorry") && strings.Contains(lower, "cancel"))
}

func isAmazonShippedEmail(html, subject string) bool {
	lowerH := strings.ToLower(html)
	lowerS := strings.ToLower(subject)
	// Subject: Shipped: "Dealusy 100 Pack - 16 oz..."
	return strings.HasPrefix(lowerS, "shipped:") ||
		strings.Contains(lowerH, "has shipped") ||
		strings.Contains(lowerS, "has shipped") ||
		strings.Contains(lowerH, "your order has shipped") ||
		strings.Contains(lowerS, "your order has shipped") ||
		strings.Contains(lowerS, "shipped:")
}

func isAmazonDeliveredEmail(html, subject string) bool {
	lowerH := strings.ToLower(html)
	lowerS := strings.ToLower(subject)
	// Subject: Delivered: "Dealusy 100 Pack - 16 oz..."
	return strings.HasPrefix(lowerS, "delivered:") ||
		strings.Contains(lowerH, "has been delivered") ||
		strings.Contains(lowerS, "has been delivered") ||
		strings.Contains(lowerH, "your package has been delivered") ||
		strings.Contains(lowerS, "delivered:") ||
		strings.Contains(lowerS, "your amazon package has been delivered")
}

func parseAmazonOrderNumberFromSubject(subject string) string {
	// Amazon order ID: 123-4567890-1234567
	re := regexp.MustCompile(`(\d{3}-\d{7}-\d{7})`)
	if m := re.FindStringSubmatch(subject); len(m) > 1 {
		return strings.TrimSpace(m[1])
	}
	re2 := regexp.MustCompile(`(?i)order\s*#?\s*:?\s*(\d{3}-\d{6,7}-\d{6,7})`)
	if m := re2.FindStringSubmatch(subject); len(m) > 1 {
		return strings.TrimSpace(m[1])
	}
	return ""
}

func parseAmazonTrackingNumber(html string) string {
	// Tracking number in Amazon emails (UPS, FedEx, USPS, etc.)
	re := regexp.MustCompile(`(?i)tracking\s*(?:number|#)?\s*:?\s*([A-Z0-9]{10,30})`)
	if m := re.FindStringSubmatch(html); len(m) > 1 {
		return strings.TrimSpace(m[1])
	}
	re2 := regexp.MustCompile(`(?i)(?:ups|fedex|usps)\s*(?:tracking)?\s*#?\s*:?\s*([A-Z0-9]{10,30})`)
	if m := re2.FindStringSubmatch(html); len(m) > 1 {
		return strings.TrimSpace(m[1])
	}
	return ""
}

func parseAmazonOrderFromHTML(html string) *AmazonOrderInfo {
	info := &AmazonOrderInfo{}

	// Order number: <span>112-5870622-1457826</span> or plain 112-5870622-1457826
	orderNumInSpan := regexp.MustCompile(`<span[^>]*>\s*[^<]*?(\d{3}-\d{7}-\d{7})[^<]*\s*</span>`)
	if m := orderNumInSpan.FindStringSubmatch(html); len(m) > 1 {
		info.OrderNumber = strings.TrimSpace(m[1])
	}
	if info.OrderNumber == "" {
		orderNumRe := regexp.MustCompile(`(\d{3}-\d{7}-\d{7})`)
		if m := orderNumRe.FindStringSubmatch(html); len(m) > 1 {
			info.OrderNumber = strings.TrimSpace(m[1])
		}
	}
	if info.OrderNumber == "" {
		orderNumRe2 := regexp.MustCompile(`(?i)order\s*(?:number|#|id)?\s*:?\s*(\d{3}-\d{6,7}-\d{6,7})`)
		if m := orderNumRe2.FindStringSubmatch(html); len(m) > 1 {
			info.OrderNumber = strings.TrimSpace(m[1])
		}
	}

	// Order total: <td align="right" ... font-weight:bold>... $22.08</td>
	orderTotalInTd := regexp.MustCompile(`<td[^>]*align="right"[^>]*>[\s\S]*?\$\s*([\d,]+\.[0-9]{2})`)
	if m := orderTotalInTd.FindStringSubmatch(html); len(m) > 1 {
		info.OrderTotal = "$" + strings.TrimSpace(m[1])
	}
	if info.OrderTotal == "" {
		orderTotalRe := regexp.MustCompile(`(?i)order\s+total\s*:?\s*\$?\s*([\d,]+\.[0-9]{2})`)
		if m := orderTotalRe.FindStringSubmatch(html); len(m) > 1 {
			info.OrderTotal = "$" + strings.TrimSpace(m[1])
		}
	}
	if info.OrderTotal == "" {
		orderTotalRe2 := regexp.MustCompile(`(?i)total\s*(?:cost)?\s*:?\s*\$?\s*([\d,]+\.[0-9]{2})`)
		if m := orderTotalRe2.FindStringSubmatch(html); len(m) > 1 {
			info.OrderTotal = "$" + strings.TrimSpace(m[1])
		}
	}
	if info.OrderTotal == "" {
		orderTotalRe3 := regexp.MustCompile(`\$([\d,]+\.[0-9]{2})`)
		if all := orderTotalRe3.FindAllStringSubmatch(html, -1); len(all) > 0 {
			info.OrderTotal = "$" + strings.TrimSpace(all[len(all)-1][1])
		}
	}

	// Ship to / delivery address
	deliversRe := regexp.MustCompile(`(?is)(?:ship\s+to|delivery\s+address)\s*:?[\s\S]*?([A-Za-z0-9\s,\.\-]+(?:Street|St|Ave|Avenue|Blvd|Road|Rd|Drive|Dr|Lane|Ln)[^<]{10,200})`)
	if m := deliversRe.FindStringSubmatch(html); len(m) > 1 {
		info.DeliversTo = strings.TrimSpace(m[1])
		info.DeliversTo = regexp.MustCompile(`\s+`).ReplaceAllString(info.DeliversTo, " ")
	}
	if info.DeliversTo == "" {
		deliversRe2 := regexp.MustCompile(`(?is)ship\s+to\s*:?\s*([^<\n]{15,200})`)
		if m := deliversRe2.FindStringSubmatch(html); len(m) > 1 {
			info.DeliversTo = strings.TrimSpace(m[1])
			info.DeliversTo = regexp.MustCompile(`\s+`).ReplaceAllString(info.DeliversTo, " ")
		}
	}
	if info.DeliversTo != "" && !looksLikeAddress(info.DeliversTo) {
		info.DeliversTo = ""
	}

	// Product image first (we need it to get alt for full product name)
	info.ProductImage, info.ProductName = parseAmazonProductImageAndAlt(html)

	// Product name: prefer alt from product image (full name); else <a href=".../dp/...">text</a>
	if info.ProductName == "" {
		productReWithDp := regexp.MustCompile(`(?is)<a[^>]+href="([^"]*)"[^>]*>([^<]+)</a>`)
		for _, m := range productReWithDp.FindAllStringSubmatch(html, -1) {
			if len(m) < 3 {
				continue
			}
			href, text := m[1], strings.TrimSpace(m[2])
			if !strings.Contains(strings.ToLower(href), "amazon") {
				continue
			}
			if !strings.Contains(href, "/dp/") && !strings.Contains(href, "%2Fdp%2F") {
				continue
			}
			textLower := strings.ToLower(text)
			if textLower == "your orders" || strings.HasPrefix(textLower, "view ") || strings.HasPrefix(textLower, "track ") {
				continue
			}
			if len(text) > 2 && len(text) < 300 {
				info.ProductName = text
				break
			}
		}
	}
	if info.ProductName == "" {
		productRe := regexp.MustCompile(`(?is)<a[^>]+href="[^"]*amazon[^"]*"[^>]*>([^<]+)</a>`)
		if m := productRe.FindStringSubmatch(html); len(m) > 1 {
			t := strings.TrimSpace(m[1])
			tLower := strings.ToLower(t)
			if tLower != "your orders" && len(t) > 2 && len(t) < 300 && !strings.HasPrefix(tLower, "view ") && !strings.HasPrefix(tLower, "track ") {
				info.ProductName = t
			}
		}
	}
	if info.ProductName == "" {
		productRe2 := regexp.MustCompile(`(?is)item\s*:?\s*([^<\n]{10,150})`)
		if m := productRe2.FindStringSubmatch(html); len(m) > 1 {
			info.ProductName = strings.TrimSpace(m[1])
		}
	}

	// Qty: <span>Quantity: 1</span> or <div...><span>Quantity: 1</span></div>
	qtyRe := regexp.MustCompile(`(?i)quantity\s*:\s*(\d+)`)
	if m := qtyRe.FindStringSubmatch(html); len(m) > 1 {
		info.Qty = strings.TrimSpace(m[1])
	}
	if info.Qty == "" {
		qtyRe2 := regexp.MustCompile(`(?is)<span>[^<]*Quantity\s*:\s*(\d+)[^<]*</span>`)
		if m := qtyRe2.FindStringSubmatch(html); len(m) > 1 {
			info.Qty = strings.TrimSpace(m[1])
		}
	}

	return info
}

// parseAmazonProductImageAndAlt returns (fullImageSrc, productNameFromAlt). Full image src is the raw src (e.g. Gmail proxy URL).
func parseAmazonProductImageAndAlt(html string) (imageURL, productName string) {
	re := regexp.MustCompile(`(?i)<img[^>]+src="([^"]+)"[^>]*>`)
	altRe := regexp.MustCompile(`(?i)alt="([^"]*)"`)
	for _, m := range re.FindAllStringSubmatch(html, -1) {
		if len(m) < 2 {
			continue
		}
		src := strings.TrimSpace(m[1])
		srcLower := strings.ToLower(src)
		if strings.Contains(srcLower, "outbound") || strings.Contains(srcLower, "steptracker") || strings.Contains(srcLower, "checkmark") {
			continue
		}
		if strings.Contains(srcLower, "logo") || strings.Contains(srcLower, "icon") {
			continue
		}
		if !strings.Contains(src, "m.media-amazon.com/images/") {
			continue
		}
		// Prefer /images/I/ (product images); return full src (with Gmail proxy)
		if strings.Contains(src, "m.media-amazon.com/images/I/") {
			if altM := altRe.FindStringSubmatch(m[0]); len(altM) > 1 {
				productName = strings.TrimSpace(altM[1])
				if len(productName) > 15 && len(productName) < 500 {
					// full name from alt
				}
			}
			return src, productName
		}
		if imageURL == "" {
			if altM := altRe.FindStringSubmatch(m[0]); len(altM) > 1 {
				alt := strings.TrimSpace(altM[1])
				if len(alt) > 15 && len(alt) < 500 {
					productName = alt
				}
			}
			imageURL, productName = src, productName
		}
	}
	if imageURL != "" {
		return imageURL, productName
	}
	re2 := regexp.MustCompile(`(?i)src="(https://[^"]*#https://m\.media-amazon\.com/images/[^"]+)"`)
	if m := re2.FindStringSubmatch(html); len(m) > 1 {
		return strings.TrimSpace(m[1]), ""
	}
	re3 := regexp.MustCompile(`(?i)src="([^"]*m\.media-amazon\.com/images/I/[^"]+)"`)
	if m := re3.FindStringSubmatch(html); len(m) > 1 {
		return strings.TrimSpace(m[1]), ""
	}
	return "", ""
}
