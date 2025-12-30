package imapgo

import(
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
	}
	return message, err
}