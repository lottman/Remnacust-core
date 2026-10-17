// Package quotausage defines private counters used only by autonomous node billing.
package quotausage

import (
	"errors"
	"strconv"
	"strings"

	"github.com/xtls/xray-core/common/uuid"
)

const Capability = "remnacust-unreserved-usage-v1"
const Prefix = "qfree>>>"

type Counter struct {
	Account, Owner, Direction string
	Factor, Scale             int64
}

func canonicalUUID(value string) bool {
	id, err := uuid.ParseString(value)
	return err == nil && len(value) == 36 && id.String() == value && id[6]>>4 >= 1 && id[6]>>4 <= 8 && id[8]>>6 == 2
}

func decimal(value string) (int64, bool) {
	n, err := strconv.ParseInt(value, 10, 64)
	return n, err == nil && n > 0 && strconv.FormatInt(n, 10) == value
}

func (counter Counter) Name() (string, error) {
	if !canonicalUUID(counter.Account) || (counter.Scale != 100 && counter.Scale != 1000000000) || counter.Factor <= 0 || counter.Factor > 100*counter.Scale || (counter.Direction != "uplink" && counter.Direction != "downlink") {
		return "", errors.New("invalid unreserved usage counter")
	}
	if _, ok := decimal(counter.Owner); !ok {
		return "", errors.New("invalid unreserved usage owner")
	}
	return Prefix + counter.Account + ">>>" + counter.Owner + ">>>" + strconv.FormatInt(counter.Factor, 10) + ">>>" + strconv.FormatInt(counter.Scale, 10) + ">>>traffic>>>" + counter.Direction, nil
}

func Parse(name string) (Counter, error) {
	parts := strings.Split(name, ">>>")
	if len(parts) != 7 || parts[0] != "qfree" || parts[5] != "traffic" {
		return Counter{}, errors.New("invalid unreserved usage counter name")
	}
	factor, priced := decimal(parts[3])
	scale, precise := decimal(parts[4])
	value := Counter{Account: parts[1], Owner: parts[2], Factor: factor, Scale: scale, Direction: parts[6]}
	canonical, err := value.Name()
	if err != nil || !priced || !precise || canonical != name {
		return Counter{}, errors.New("invalid unreserved usage counter binding")
	}
	return value, nil
}

func IsUsageCounter(name string) bool {
	if strings.HasPrefix(name, Prefix) {
		_, err := Parse(name)
		return err == nil
	}
	if !strings.HasPrefix(name, "user>>>") {
		return false
	}
	for _, suffix := range []string{">>>traffic>>>uplink", ">>>traffic>>>downlink"} {
		if strings.HasSuffix(name, suffix) && len(name) > len("user>>>")+len(suffix) {
			return true
		}
	}
	return false
}
