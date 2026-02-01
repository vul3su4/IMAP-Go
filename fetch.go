package imapgo

import(
	"fmt"
	"time"

)

func (n *ImapOpts) FetchEmail()(string, error){
	var message string
	var err error
	switch n.Site{
	case Cityline:
		for i := 1; i < n.MaxChecks; i++ {
			message, err = n.getCitylineLoginCode()
			if err == nil{
				break
			}
			time.Sleep(5* time.Second)
		}
	case Nike:
		for i := 1; i  < n.MaxChecks; i++ {
			message, err = n.getNikeLoginCode()
			if err == nil{
				break
			}
			time.Sleep(5* time.Second)
		}
	case Target:
		for i := 1; i < n.MaxChecks; i++ {
			info, fetchErr := n.GetTargetOrderInfo()
			if fetchErr == nil && info != nil {
				message = fmt.Sprintf("OrderNumber:%s|OrderTotal:%s|DeliversTo:%s|ProductName:%s|ProductImage:%s|ReceivedAt:%d",
					info.OrderNumber, info.OrderTotal, info.DeliversTo, info.ProductName, info.ProductImage, info.ReceivedAt)
				break
			}
			err = fetchErr
			time.Sleep(5 * time.Second)
		}
	}
	return message, err
}