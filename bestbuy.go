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

const bestbuyFetchCountDefault = 50
const bestbuyStatusFetchCountDefault = 100

// GetBestBuyOrderList from last N emails find Best Buy order info
func (n *ImapOpts) GetBestBuyOrderList() ([]*BestBuyOrderInfo, error) {
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

	boxes = statusCheckMailboxes(boxes)

	var list []*BestBuyOrderInfo
	fetchCount := bestbuyFetchCountDefault
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
			mailsubjectLower := strings.ToLower(mailsubject)
			if !isBestBuyOrderRelatedSubject(mailsubjectLower) {
				continue
			}

			var fromaddress string
			if fromList, err := header.AddressList("From"); err == nil && len(fromList) > 0 {
				fromaddress = fromList[0].String()
			}
			fromLower := strings.ToLower(fromaddress)
			if !strings.Contains(fromLower, "bestbuy.com") {
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
						strings.Contains(bodyLower, "bestbuy") ||
						strings.Contains(bodyLower, "bby01")) {
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

			info := parseBestBuyOrderFromHTML(htmlBody)
			if info.OrderNumber == "" {
				info.OrderNumber = parseBestBuyOrderNumberFromSubject(mailsubject)
			}
			if info.OrderNumber == "" {
				continue
			}
			info.ReceivedAt = maildate
			list = append(list, info)
		}
	}

	list = dedupeBestBuyOrdersByOrderNumber(list)
	return list, nil
}

func dedupeBestBuyOrdersByOrderNumber(list []*BestBuyOrderInfo) []*BestBuyOrderInfo {
	seen := make(map[string]bool)
	var out []*BestBuyOrderInfo
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

// GetBestBuyOrderInfo return the first Best Buy order (compatible with FetchEmail usage)
func (n *ImapOpts) GetBestBuyOrderInfo() (*BestBuyOrderInfo, error) {
	list, err := n.GetBestBuyOrderList()
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, errors.New("no order found")
	}
	return list[0], nil
}

// GetBestBuyOrderStatus check order status from recent Best Buy emails
func (n *ImapOpts) GetBestBuyOrderStatus(orderNumber string) (*BestBuyOrderStatus, error) {
	orderNumber = strings.TrimSpace(orderNumber)
	if orderNumber == "" {
		return &BestBuyOrderStatus{OrderNumber: "", Status: "unknown", ReceivedAt: 0}, nil
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
	fetchCount := bestbuyStatusFetchCountDefault
	if n.TargetFetchCount > 0 {
		fetchCount = n.TargetFetchCount
		if fetchCount < 50 {
			fetchCount = 100
		}
	}

	var latestConfirmedAt int64
	var latestConfirmedSubject string
	var latestShippedAt int64
	var latestShippedSubject string
	var latestShippedTracking string
	var latestDeliveredAt int64
	var latestDeliveredSubject string

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
			if !isBestBuyOrderRelatedSubject(mailsubjectLower) {
				continue
			}
			var fromaddress string
			if fromList, err := header.AddressList("From"); err == nil && len(fromList) > 0 {
				fromaddress = fromList[0].String()
			}
			if !strings.Contains(strings.ToLower(fromaddress), "bestbuy.com") {
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
					// Prefer HTML part (shipped email often has text/plain first, then text/html)
					if strings.Contains(body, "</") || strings.Contains(body, "<h1") {
						htmlBody = body
						break
					}
					if htmlBody == "" {
						htmlBody = body
					}
				}
			}
			if htmlBody == "" {
				continue
			}

			orderInSubject := strings.Contains(mailsubject, orderNumber)
			orderInBody := strings.Contains(htmlBody, orderNumber)
			if !orderInSubject && !orderInBody {
				// Shipped email may only have digits (e.g. 807139794433) without BBY01-
				digits := regexp.MustCompile(`\D`).ReplaceAllString(orderNumber, "")
				if digits == "" || (!strings.Contains(htmlBody, digits) && !strings.Contains(mailsubject, digits)) {
					continue
				}
			}

			isDelivered := isBestBuyDeliveredEmail(htmlBody)
			isShipped := isBestBuyShippedEmail(htmlBody)
			if isDelivered {
				if maildate > latestDeliveredAt {
					latestDeliveredAt = maildate
					latestDeliveredSubject = mailsubject
				}
			} else if isShipped {
				if maildate > latestShippedAt {
					latestShippedAt = maildate
					latestShippedSubject = mailsubject
					latestShippedTracking = parseBestBuyTrackingNumber(htmlBody)
				}
			} else if maildate > latestConfirmedAt {
				latestConfirmedAt = maildate
				latestConfirmedSubject = mailsubject
			}
		}
	}

	res := &BestBuyOrderStatus{
		OrderNumber:    orderNumber,
		Status:         "unknown",
		ReceivedAt:     0,
		Subject:        "",
		TrackingNumber: "",
	}
	if latestDeliveredAt > 0 {
		res.Status = "Delivered"
		res.ReceivedAt = latestDeliveredAt
		res.Subject = latestDeliveredSubject
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

func isBestBuyOrderRelatedSubject(subject string) bool {
	lower := strings.ToLower(subject)
	return strings.Contains(lower, "order") ||
		strings.Contains(lower, "best buy") ||
		strings.Contains(lower, "bestbuy") ||
		strings.Contains(lower, "bby01") ||
		strings.Contains(lower, "shipped") ||
		strings.Contains(lower, "package") ||
		strings.Contains(lower, "delivered") ||
		strings.Contains(lower, "on its way")
}

// isBestBuyShippedEmail: <h1 ...>Your package is on its way.</h1>
func isBestBuyShippedEmail(html string) bool {
	lower := strings.ToLower(html)
	return strings.Contains(lower, "your package is on its way") ||
		strings.Contains(lower, "package is on its way")
}

// isBestBuyDeliveredEmail: "Your package has been delivered" or <span class="il">Delivered</span>
func isBestBuyDeliveredEmail(html string) bool {
	lower := strings.ToLower(html)
	return strings.Contains(lower, "your package has been delivered") ||
		strings.Contains(lower, "has been delivered") ||
		(strings.Contains(lower, "good news") && strings.Contains(lower, "delivered"))
}

// parseBestBuyTrackingNumber: <span ...>498607254739</span> (after "Your package is on its way")
func parseBestBuyTrackingNumber(html string) string {
	// Prefer span with only digits after "Your package is on its way"
	idx := strings.Index(html, "Your package is on its way")
	if idx >= 0 {
		after := html[idx:]
		re := regexp.MustCompile(`<span[^>]*>\s*(\d{10,20})\s*</span>`)
		if m := re.FindStringSubmatch(after); len(m) > 1 {
			return strings.TrimSpace(m[1])
		}
	}
	re := regexp.MustCompile(`<span[^>]*>\s*(\d{10,20})\s*</span>`)
	if m := re.FindStringSubmatch(html); len(m) > 1 {
		return strings.TrimSpace(m[1])
	}
	return ""
}

func parseBestBuyOrderNumberFromSubject(subject string) string {
	// Best Buy order ID: BBY01-807139794433
	re := regexp.MustCompile(`(?i)(BBY01-\d{9,15})`)
	if m := re.FindStringSubmatch(subject); len(m) > 1 {
		return strings.TrimSpace(m[1])
	}
	return ""
}

func parseBestBuyOrderFromHTML(html string) *BestBuyOrderInfo {
	info := &BestBuyOrderInfo{}

	// Order number: <span ...>BBY01-807139794433</span>
	orderNumRe := regexp.MustCompile(`<span[^>]*>\s*(BBY01-\d{9,15})\s*</span>`)
	if m := orderNumRe.FindStringSubmatch(html); len(m) > 1 {
		info.OrderNumber = strings.TrimSpace(m[1])
	}
	if info.OrderNumber == "" {
		orderNumRe2 := regexp.MustCompile(`(?i)(BBY01-\d{9,15})`)
		if m := orderNumRe2.FindStringSubmatch(html); len(m) > 1 {
			info.OrderNumber = strings.TrimSpace(m[1])
		}
	}

	// Delivers to: <strong>Shipped to:</strong><br><br>Yen Long Chen<br>1530 Belmont Ave<br>Seattle, WA 98122
	shippedToRe := regexp.MustCompile(`(?is)<strong>\s*Shipped to:\s*</strong>\s*<br>\s*<br>([^<]+)<br>([^<]+)<br>([^<]+)`)
	if m := shippedToRe.FindStringSubmatch(html); len(m) > 3 {
		parts := []string{strings.TrimSpace(m[1]), strings.TrimSpace(m[2]), strings.TrimSpace(m[3])}
		info.DeliversTo = strings.Join(parts, ", ")
		info.DeliversTo = regexp.MustCompile(`\s+`).ReplaceAllString(info.DeliversTo, " ")
	}
	if info.DeliversTo == "" {
		shippedToRe2 := regexp.MustCompile(`(?is)Shipped to:\s*</strong>\s*<br[^>]*>\s*<br[^>]*>([\s\S]*?)</span>`)
		if m := shippedToRe2.FindStringSubmatch(html); len(m) > 1 {
			addr := strings.TrimSpace(m[1])
			addr = regexp.MustCompile(`<br\s*/?>`).ReplaceAllString(addr, ", ")
			addr = regexp.MustCompile(`<[^>]+>`).ReplaceAllString(addr, " ")
			addr = regexp.MustCompile(`\s+`).ReplaceAllString(addr, " ")
			if looksLikeAddress(addr) {
				info.DeliversTo = addr
			}
		}
	}

	// Order total: <td align="right" ...>$11.03</td>
	orderTotalRe := regexp.MustCompile(`<td[^>]*align="right"[^>]*>[\s\S]*?\$\s*([\d,]+\.[0-9]{2})\s*</td>`)
	if m := orderTotalRe.FindStringSubmatch(html); len(m) > 1 {
		info.OrderTotal = "$" + strings.TrimSpace(m[1])
	}
	if info.OrderTotal == "" {
		orderTotalRe2 := regexp.MustCompile(`(?i)order\s+total\s*:?\s*\$?\s*([\d,]+\.[0-9]{2})`)
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

	// Product name: <a href="...click.emailinfo2.bestbuy.com...">Riftbound - League of Legends...</a>
	// Prefer link with long text (product name); skip nav text like "Web", "View", "Track"
	productRe := regexp.MustCompile(`(?is)<a[^>]+href="([^"]*)"[^>]*>([^<]+)</a>`)
	skipText := map[string]bool{"web": true, "view": true, "track": true, "order": true, "account": true}
	for _, m := range productRe.FindAllStringSubmatch(html, -1) {
		if len(m) < 3 {
			continue
		}
		href, text := strings.TrimSpace(m[1]), strings.TrimSpace(m[2])
		if !strings.Contains(strings.ToLower(href), "bestbuy") {
			continue
		}
		textLower := strings.ToLower(text)
		if skipText[textLower] || len(textLower) < 10 {
			continue
		}
		if len(text) > len(info.ProductName) && len(text) < 500 {
			info.ProductName = text
		}
	}

	// Product image: <img src="...#https://pisces.bbystatic.com/...">
	info.ProductImage = parseBestBuyProductImageURL(html)

	// Qty: <td style="padding-bottom:20px;padding-right:8px">2</td> - td with only digits (no $)
	qtyRe := regexp.MustCompile(`<td[^>]*padding-bottom:20px[^>]*>\s*(\d+)\s*</td>`)
	if m := qtyRe.FindStringSubmatch(html); len(m) > 1 {
		info.Qty = strings.TrimSpace(m[1])
	}
	if info.Qty == "" {
		// Fallback: td that contains only digits (short, not order total row)
		qtyRe2 := regexp.MustCompile(`<td[^>]*>\s*(\d{1,3})\s*</td>`)
		for _, m := range qtyRe2.FindAllStringSubmatch(html, -1) {
			// Skip if this looks like part of order total (e.g. 11.03 has digits)
			q := strings.TrimSpace(m[1])
			if q != "" && len(q) <= 3 {
				info.Qty = q
				break
			}
		}
	}

	return info
}

// parseBestBuyProductImageURL: img src with Gmail proxy #https://pisces.bbystatic.com/...
func parseBestBuyProductImageURL(html string) string {
	re := regexp.MustCompile(`(?i)<img[^>]+src="([^"]+)"[^>]*>`)
	for _, m := range re.FindAllStringSubmatch(html, -1) {
		if len(m) < 2 {
			continue
		}
		src := strings.TrimSpace(m[1])
		srcLower := strings.ToLower(src)
		if !strings.Contains(src, "bbystatic.com") {
			continue
		}
		if strings.Contains(srcLower, "logo") || strings.Contains(srcLower, "icon") {
			continue
		}
		return src
	}
	re2 := regexp.MustCompile(`(?i)src="([^"]*bbystatic\.com[^"]+)"`)
	if m := re2.FindStringSubmatch(html); len(m) > 1 {
		return strings.TrimSpace(m[1])
	}
	return ""
}
