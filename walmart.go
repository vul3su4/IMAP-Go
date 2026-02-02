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

const walmartFetchCountDefault = 50
const walmartStatusFetchCountDefault = 100

// GetWalmartOrderList from last N emails find Walmart order info (delivery order confirmation)
func (n *ImapOpts) GetWalmartOrderList() ([]*WalmartOrderInfo, error) {
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

	var list []*WalmartOrderInfo
	fetchCount := walmartFetchCountDefault
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
			if !isWalmartOrderRelatedSubject(mailsubjectLower) {
				continue
			}

			var fromaddress string
			if fromList, err := header.AddressList("From"); err == nil && len(fromList) > 0 {
				fromaddress = fromList[0].String()
			}
			fromLower := strings.ToLower(fromaddress)
			if !strings.Contains(fromLower, "walmart.com") {
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
					if hasTags && (strings.Contains(bodyLower, "delivery order") ||
						strings.Contains(bodyLower, "order total") ||
						strings.Contains(bodyLower, "walmart")) {
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

			if isWalmartCancellationSubject(mailsubject) {
				continue
			}

			info := parseWalmartOrderFromHTML(htmlBody)
			if info.OrderNumber == "" {
				info.OrderNumber = parseWalmartOrderNumberFromSubject(mailsubject)
			}
			if info.OrderNumber == "" {
				continue
			}
			info.ReceivedAt = maildate
			list = append(list, info)
		}
	}

	list = dedupeWalmartOrdersByOrderNumber(list)
	return list, nil
}

func dedupeWalmartOrdersByOrderNumber(list []*WalmartOrderInfo) []*WalmartOrderInfo {
	seen := make(map[string]bool)
	var out []*WalmartOrderInfo
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

// GetWalmartOrderInfo return the first Walmart order (compatible with FetchEmail usage)
func (n *ImapOpts) GetWalmartOrderInfo() (*WalmartOrderInfo, error) {
	list, err := n.GetWalmartOrderList()
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, errors.New("no order found")
	}
	return list[0], nil
}

// GetWalmartOrderStatus check order status from recent Walmart emails
func (n *ImapOpts) GetWalmartOrderStatus(orderNumber string) (*WalmartOrderStatus, error) {
	orderNumber = strings.TrimSpace(orderNumber)
	if orderNumber == "" {
		return &WalmartOrderStatus{OrderNumber: "", Status: "unknown", ReceivedAt: 0}, nil
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
	fetchCount := walmartStatusFetchCountDefault
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
			if !isWalmartOrderRelatedSubject(mailsubjectLower) {
				continue
			}
			var fromaddress string
			if fromList, err := header.AddressList("From"); err == nil && len(fromList) > 0 {
				fromaddress = fromList[0].String()
			}
			if !strings.Contains(strings.ToLower(fromaddress), "walmart.com") {
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

			if !strings.Contains(mailsubject, orderNumber) && !strings.Contains(htmlBody, orderNumber) && !walmartSubjectOrderNumberMatches(mailsubject, orderNumber) {
				continue
			}

			isCanceled := isWalmartCancellationSubject(mailsubject)
			isShipped := isWalmartShippedSubject(mailsubject)
			// "Thanks for your delivery order" = order confirmation (confirmed), NOT delivered; only "Delivered: ..." / "has been delivered" = item arrived
			isDelivered := (strings.HasPrefix(mailsubjectLower, "delivered:") ||
				strings.Contains(mailsubjectLower, "has been delivered") ||
				strings.Contains(mailsubjectLower, "was delivered")) && !isCanceled

			if isCanceled {
				if maildate > latestCanceledAt {
					latestCanceledAt = maildate
					latestCanceledSubject = mailsubject
				}
			} else if isDelivered {
				if maildate > latestDeliveredAt {
					latestDeliveredAt = maildate
					latestDeliveredSubject = mailsubject
				}
			} else if isShipped {
				if maildate > latestShippedAt {
					latestShippedAt = maildate
					latestShippedSubject = mailsubject
					latestShippedTracking = parseWalmartTrackingNumber(htmlBody)
				}
			} else {
				if maildate > latestConfirmedAt {
					latestConfirmedAt = maildate
					latestConfirmedSubject = mailsubject
				}
			}
		}
	}

	res := &WalmartOrderStatus{
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

func isWalmartOrderRelatedSubject(subject string) bool {
	lower := strings.ToLower(subject)
	return strings.Contains(lower, "thanks for your delivery order") ||
		strings.Contains(lower, "delivery order") ||
		strings.Contains(lower, "canceled") ||
		strings.HasPrefix(lower, "shipped:") ||
		strings.Contains(lower, "shipped") ||
		strings.Contains(lower, "order") ||
		strings.Contains(lower, "walmart")
}

// isWalmartShippedSubject: "Shipped: Pokemon Trading Card G... and 4 other items"
func isWalmartShippedSubject(subject string) bool {
	lower := strings.ToLower(subject)
	return strings.HasPrefix(lower, "shipped:") || strings.Contains(lower, "shipped:")
}

// parseWalmartTrackingNumber: "Fedex tracking number <a href=\"...\">398225005502</a>" -> "Fedex 398225005502"
func parseWalmartTrackingNumber(html string) string {
	// (Fedex|UPS|USPS|...) tracking number ... <a ...>398225005502</a>
	re := regexp.MustCompile(`(?i)(fedex|ups|usps|ontrac|dhl)\s+tracking\s*number[\s\S]*?<a[^>]*>(\d{10,20})</a>`)
	if m := re.FindStringSubmatch(html); len(m) > 2 {
		carrier := walmartTrackingCarrierName(m[1])
		num := strings.TrimSpace(m[2])
		if carrier != "" {
			return carrier + " " + num
		}
		return num
	}
	// Fedex tracking number (no capture) ... <a ...>398225005502</a>
	re2 := regexp.MustCompile(`(?i)(fedex|ups|usps|ontrac|dhl)\s+tracking\s*number[\s\S]*?<a[^>]*>(\d+)</a>`)
	if m := re2.FindStringSubmatch(html); len(m) > 2 {
		carrier := walmartTrackingCarrierName(m[1])
		num := strings.TrimSpace(m[2])
		if carrier != "" {
			return carrier + " " + num
		}
		return num
	}
	// Fallback: tracking number ... <a>...</a> (no carrier in pattern)
	re3 := regexp.MustCompile(`(?i)tracking\s*number[\s\S]*?<a[^>]*>(\d+)</a>`)
	if m := re3.FindStringSubmatch(html); len(m) > 1 {
		return strings.TrimSpace(m[1])
	}
	return ""
}

func walmartTrackingCarrierName(s string) string {
	lower := strings.ToLower(strings.TrimSpace(s))
	switch lower {
	case "fedex":
		return "Fedex"
	case "ups":
		return "UPS"
	case "usps":
		return "USPS"
	case "ontrac":
		return "OnTrac"
	case "dhl":
		return "DHL"
	default:
		return ""
	}
}

// isWalmartCancellationSubject: "Canceled: delivery from order #200014293466922"
func isWalmartCancellationSubject(subject string) bool {
	lower := strings.ToLower(subject)
	return strings.HasPrefix(lower, "canceled:") ||
		(strings.Contains(lower, "canceled") && strings.Contains(lower, "delivery") && strings.Contains(lower, "order"))
}

// walmartSubjectOrderNumberMatches: subject "order #200014293466922" matches orderNumber "200014293466922" or "2000142-14315504" (digits only compare)
func walmartSubjectOrderNumberMatches(subject, orderNumber string) bool {
	re := regexp.MustCompile(`(?i)order\s*#\s*(\d+)`)
	m := re.FindStringSubmatch(subject)
	if len(m) < 2 {
		return false
	}
	subjectDigits := m[1]
	orderDigits := regexp.MustCompile(`\D`).ReplaceAllString(orderNumber, "")
	return subjectDigits == orderDigits
}

func parseWalmartOrderNumberFromSubject(subject string) string {
	// Walmart order ID: 2000142-14315504 (7-8 digits)
	re := regexp.MustCompile(`(\d{7}-\d{8})`)
	if m := re.FindStringSubmatch(subject); len(m) > 1 {
		return strings.TrimSpace(m[1])
	}
	re2 := regexp.MustCompile(`(?i)order\s*#?\s*:?\s*(\d{7}-\d{8})`)
	if m := re2.FindStringSubmatch(subject); len(m) > 1 {
		return strings.TrimSpace(m[1])
	}
	return ""
}

func parseWalmartOrderFromHTML(html string) *WalmartOrderInfo {
	info := &WalmartOrderInfo{}

	// Order number: <span class="il">2000142-14315504</span>
	orderNumInSpan := regexp.MustCompile(`<span[^>]*class="il"[^>]*>\s*(\d{7}-\d{8})\s*</span>`)
	if m := orderNumInSpan.FindStringSubmatch(html); len(m) > 1 {
		info.OrderNumber = strings.TrimSpace(m[1])
	}
	if info.OrderNumber == "" {
		orderNumRe := regexp.MustCompile(`(\d{7}-\d{8})`)
		if m := orderNumRe.FindStringSubmatch(html); len(m) > 1 {
			info.OrderNumber = strings.TrimSpace(m[1])
		}
	}

	// Order total: <td width="25%" align="right"> <strong>$110.38</strong></td>
	orderTotalRe := regexp.MustCompile(`<td[^>]*align="right"[^>]*>[\s\S]*?<strong>\s*\$([\d,]+\.[0-9]{2})\s*</strong>`)
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

	// Delivers to: <p style="color:#2e2f32;margin:0">YCBS 1530 Belmont Ave, APT 205 CDDD, Seattle, WA, 98122, USA</p>
	deliversRe := regexp.MustCompile(`<p[^>]*style="[^"]*"[^>]*>([^<]{15,300})</p>`)
	for _, m := range deliversRe.FindAllStringSubmatch(html, -1) {
		addr := strings.TrimSpace(m[1])
		addr = regexp.MustCompile(`\s+`).ReplaceAllString(addr, " ")
		if looksLikeAddress(addr) {
			info.DeliversTo = addr
			break
		}
	}
	if info.DeliversTo == "" {
		deliversRe2 := regexp.MustCompile(`<p[^>]*>([^<]{15,300})</p>`)
		for _, m := range deliversRe2.FindAllStringSubmatch(html, -1) {
			addr := strings.TrimSpace(m[1])
			if looksLikeAddress(addr) {
				info.DeliversTo = regexp.MustCompile(`\s+`).ReplaceAllString(addr, " ")
				break
			}
		}
	}

	// Product image + alt: img with walmartimages.com; alt contains "quantity N item ProductName"
	info.ProductImage, info.ProductName, info.Qty = parseWalmartProductImageAndAlt(html)

	// Qty fallback: quantity 5 item in text
	if info.Qty == "" {
		qtyRe := regexp.MustCompile(`(?i)quantity\s*(\d+)\s*item`)
		if m := qtyRe.FindStringSubmatch(html); len(m) > 1 {
			info.Qty = strings.TrimSpace(m[1])
		}
	}

	return info
}

// parseWalmartProductImageAndAlt returns (fullImageSrc, productNameFromAlt, qty).
// img alt like: "quantity 5 item Pokemon Trading Card Game Mega Evolution..."
func parseWalmartProductImageAndAlt(html string) (imageURL, productName, qty string) {
	re := regexp.MustCompile(`(?i)<img[^>]+src="([^"]+)"[^>]*>`)
	altRe := regexp.MustCompile(`(?i)alt="([^"]*)"`)
	qtyAltRe := regexp.MustCompile(`(?i)quantity\s*(\d+)\s*item\s*(.*)`)
	for _, m := range re.FindAllStringSubmatch(html, -1) {
		if len(m) < 2 {
			continue
		}
		src := strings.TrimSpace(m[1])
		srcLower := strings.ToLower(src)
		if !strings.Contains(src, "walmartimages.com") {
			continue
		}
		if strings.Contains(srcLower, "logo") || strings.Contains(srcLower, "icon") {
			continue
		}
		var alt string
		if altM := altRe.FindStringSubmatch(m[0]); len(altM) > 1 {
			alt = strings.TrimSpace(altM[1])
		}
		if qtyAltRe.MatchString(alt) {
			parts := qtyAltRe.FindStringSubmatch(alt)
			if len(parts) >= 2 {
				qty = strings.TrimSpace(parts[1])
			}
			if len(parts) >= 3 {
				name := strings.TrimSpace(parts[2])
				if len(name) > 5 && len(name) < 500 {
					productName = name
				}
			}
			imageURL = src
			return imageURL, productName, qty
		}
		if imageURL == "" {
			imageURL = src
			if productName == "" && len(alt) > 15 && len(alt) < 500 {
				productName = alt
			}
		}
	}
	return imageURL, productName, qty
}
