package aghnet

import (
	"bufio"
	"fmt"
	"io"
	"io/fs"
	"net"
	"path"
	"strings"
	"sync"

	"github.com/AdguardTeam/AdGuardHome/internal/aghos"
	"github.com/AdguardTeam/golibs/errors"
	"github.com/AdguardTeam/golibs/log"
	"github.com/AdguardTeam/golibs/netutil"
	"github.com/AdguardTeam/golibs/stringutil"
	"github.com/AdguardTeam/urlfilter"
	"github.com/AdguardTeam/urlfilter/filterlist"
	"github.com/AdguardTeam/urlfilter/rules"
	"github.com/miekg/dns"
)

// DefaultHostsPaths returns the slice of paths default for the operating system
// to files and directories which are containing the hosts database.  The result
// is intended to be used within fs.FS so the initial slash is omitted.
func DefaultHostsPaths() (paths []string) {
	return defaultHostsPaths()
}

// requestMatcher combines the logic for matching requests and translating the
// appropriate rules.
type requestMatcher struct {
	// stateLock protects all the fields of requestMatcher.
	stateLock *sync.RWMutex

	// rulesStrg stores the rules obtained from the hosts' file.
	rulesStrg *filterlist.RuleStorage
	// engine serves rulesStrg.
	engine *urlfilter.DNSEngine

	// translator maps generated $dnsrewrite rules into hosts-syntax rules.
	//
	// TODO(e.burkov):  Store the filename from which the rule was parsed.
	translator map[string]string
}

// MatchRequest processes the request rewriting hostnames and addresses read
// from the operating system's hosts files.
//
// res is nil for any request having not an A/AAAA or PTR type.  Results
// containing CNAME information may be queried again with the same question type
// and the returned CNAME for Host field of request.  Results are guaranteed to
// be direct, i.e. any returned CNAME resolves into actual address like an alias
// in hosts does, see man hosts (5).
//
// It's safe for concurrent use.
func (rm *requestMatcher) MatchRequest(
	req urlfilter.DNSRequest,
) (res *urlfilter.DNSResult, ok bool) {
	switch req.DNSType {
	case dns.TypeA, dns.TypeAAAA, dns.TypePTR:
		log.Debug("%s: handling the request", hostsContainerPref)
	default:
		return nil, false
	}

	rm.stateLock.RLock()
	defer rm.stateLock.RUnlock()

	return rm.engine.MatchRequest(req)
}

// Translate returns the source hosts-syntax rule for the generated dnsrewrite
// rule or an empty string if the last doesn't exist.  The returned rules are in
// a processed format like:
//
//   ip host1 host2 ...
//
func (rm *requestMatcher) Translate(rule string) (hostRule string) {
	rm.stateLock.RLock()
	defer rm.stateLock.RUnlock()

	return rm.translator[rule]
}

// resetEng updates container's engine and the translation map.
func (rm *requestMatcher) resetEng(rulesStrg *filterlist.RuleStorage, tr map[string]string) {
	rm.stateLock.Lock()
	defer rm.stateLock.Unlock()

	rm.rulesStrg = rulesStrg
	rm.engine = urlfilter.NewDNSEngine(rm.rulesStrg)

	rm.translator = tr
}

// hostsContainerPref is a prefix for logging and wrapping errors in
// HostsContainer's methods.
const hostsContainerPref = "hosts container"

// HostsContainer stores the relevant hosts database provided by the OS and
// processes both A/AAAA and PTR DNS requests for those.
type HostsContainer struct {
	// requestMatcher matches the requests and translates the rules.  It's
	// embedded to implement MatchRequest and Translate for *HostsContainer.
	//
	// TODO(a.garipov, e.burkov): Consider fully merging into HostsContainer.
	requestMatcher

	// done is the channel to sign closing the container.
	done chan struct{}

	// updates is the channel for receiving updated hosts.
	updates chan *netutil.IPMap

	// last is the set of hosts that was cached within last detected change.
	last *netutil.IPMap

	// fsys is the working file system to read hosts files from.
	fsys fs.FS

	// w tracks the changes in specified files and directories.
	w aghos.FSWatcher

	// patterns stores specified paths in the fs.Glob-compatible form.
	patterns []string

	// listID is the identifier for the list of generated rules.
	listID int
}

// ErrNoHostsPaths is returned when there are no valid paths to watch passed to
// the HostsContainer.
const ErrNoHostsPaths errors.Error = "no valid paths to hosts files provided"

// NewHostsContainer creates a container of hosts, that watches the paths with
// w.  listID is used as an identifier of the underlying rules list.  paths
// shouldn't be empty and each of paths should locate either a file or a
// directory in fsys.  fsys and w must be non-nil.
func NewHostsContainer(
	listID int,
	fsys fs.FS,
	w aghos.FSWatcher,
	paths ...string,
) (hc *HostsContainer, err error) {
	defer func() { err = errors.Annotate(err, "%s: %w", hostsContainerPref) }()

	if len(paths) == 0 {
		return nil, ErrNoHostsPaths
	}

	var patterns []string
	patterns, err = pathsToPatterns(fsys, paths)
	if err != nil {
		return nil, err
	} else if len(patterns) == 0 {
		return nil, ErrNoHostsPaths
	}

	hc = &HostsContainer{
		requestMatcher: requestMatcher{
			stateLock: &sync.RWMutex{},
		},
		listID:   listID,
		done:     make(chan struct{}, 1),
		updates:  make(chan *netutil.IPMap, 1),
		fsys:     fsys,
		w:        w,
		patterns: patterns,
	}

	log.Debug("%s: starting", hostsContainerPref)

	// Load initially.
	if err = hc.refresh(); err != nil {
		return nil, err
	}

	for _, p := range paths {
		if err = w.Add(p); err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				return nil, fmt.Errorf("adding path: %w", err)
			}

			log.Debug("%s: %s is expected to exist but doesn't", hostsContainerPref, p)
		}
	}

	go hc.handleEvents()

	return hc, nil
}

// Close implements the io.Closer interface for *HostsContainer.  Close must
// only be called once.  The returned err is always nil.
func (hc *HostsContainer) Close() (err error) {
	log.Debug("%s: closing", hostsContainerPref)

	close(hc.done)

	return nil
}

// Upd returns the channel into which the updates are sent.  The receivable
// map's values are guaranteed to be of type of *aghnet.Hosts.
func (hc *HostsContainer) Upd() (updates <-chan *netutil.IPMap) {
	return hc.updates
}

// pathsToPatterns converts paths into patterns compatible with fs.Glob.
func pathsToPatterns(fsys fs.FS, paths []string) (patterns []string, err error) {
	for i, p := range paths {
		var fi fs.FileInfo
		fi, err = fs.Stat(fsys, p)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}

			// Don't put a filename here since it's already added by fs.Stat.
			return nil, fmt.Errorf("path at index %d: %w", i, err)
		}

		if fi.IsDir() {
			p = path.Join(p, "*")
		}

		patterns = append(patterns, p)
	}

	return patterns, nil
}

// handleEvents concurrently handles the file system events.  It closes the
// update channel of HostsContainer when finishes.  It's used to be called
// within a separate goroutine.
func (hc *HostsContainer) handleEvents() {
	defer log.OnPanic(fmt.Sprintf("%s: handling events", hostsContainerPref))

	defer close(hc.updates)

	ok, eventsCh := true, hc.w.Events()
	for ok {
		select {
		case _, ok = <-eventsCh:
			if !ok {
				log.Debug("%s: watcher closed the events channel", hostsContainerPref)

				continue
			}

			if err := hc.refresh(); err != nil {
				log.Error("%s: %s", hostsContainerPref, err)
			}
		case _, ok = <-hc.done:
			// Go on.
		}
	}
}

// ipRules is the pair of generated A/AAAA and PTR rules with related IP.
type ipRules struct {
	// rule is the A/AAAA $dnsrewrite rule.
	rule string
	// rulePtr is the PTR $dnsrewrite rule.
	rulePtr string
	// ip is the IP address related to the rules.
	ip net.IP
}

// hostsParser is a helper type to parse rules from the operating system's hosts
// file.  It exists for only a single refreshing session.
type hostsParser struct {
	// rulesBuilder builds the resulting rules list content.
	rulesBuilder *strings.Builder

	// rules stores the rules for main hosts to generate translations.
	rules []ipRules

	// cnameSet prevents duplicating cname rules, e.g. same hostname for
	// different IP versions.
	cnameSet *stringutil.Set

	// table stores only the unique IP-hostname pairs.  It's also sent to the
	// updates channel afterwards.
	table *netutil.IPMap
}

// newHostsParser creates a new *hostsParser with buffers of size taken from the
// previous parse.
func (hc *HostsContainer) newHostsParser() (hp *hostsParser) {
	lastLen := hc.last.Len()

	return &hostsParser{
		rulesBuilder: &strings.Builder{},
		rules:        make([]ipRules, 0, lastLen),
		cnameSet:     stringutil.NewSet(),
		table:        netutil.NewIPMap(lastLen),
	}
}

// parseFile is a aghos.FileWalker for parsing the files with hosts syntax.  It
// never signs to stop walking and never returns any additional patterns.
//
// See man hosts(5).
func (hp *hostsParser) parseFile(r io.Reader) (patterns []string, cont bool, err error) {
	s := bufio.NewScanner(r)
	for s.Scan() {
		ip, hosts := hp.parseLine(s.Text())
		if ip == nil || len(hosts) == 0 {
			continue
		}

		hp.addPairs(ip, hosts)
	}

	return nil, true, s.Err()
}

// parseLine parses the line having the hosts syntax ignoring invalid ones.
func (hp *hostsParser) parseLine(line string) (ip net.IP, hosts []string) {
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return nil, nil
	}

	if ip = net.ParseIP(fields[0]); ip == nil {
		return nil, nil
	}

	for _, f := range fields[1:] {
		hashIdx := strings.IndexByte(f, '#')
		if hashIdx == 0 {
			// The rest of the fields are a part of the comment so return.
			break
		} else if hashIdx > 0 {
			// Only a part of the field is a comment.
			f = f[:hashIdx]
		}

		// Make sure that invalid hosts aren't turned into rules.
		//
		// See https://github.com/AdguardTeam/AdGuardHome/issues/3946.
		err := netutil.ValidateDomainName(f)
		if err != nil {
			log.Error("%s: host %q is invalid, ignoring", hostsContainerPref, f)

			continue
		}

		hosts = append(hosts, f)
	}

	return ip, hosts
}

// Hosts is used to contain the main host and all it's aliases.
type Hosts struct {
	// Aliases contains all the aliases for Main.
	Aliases *stringutil.Set
	// Main is the host itself.
	Main string
}

// Equal returns true if h equals hh.
func (h *Hosts) Equal(hh *Hosts) (ok bool) {
	if h == nil || hh == nil {
		return h == hh
	}

	return h.Main == hh.Main && h.Aliases.Equal(hh.Aliases)
}

// add tries to add the ip-host pair.  It returns:
//
//   main host    if the host is not the first one added for the ip.
//   host itself  if the host is the first one added for the ip.
//   ""           if the ip-host pair has already been added.
//
func (hp *hostsParser) add(ip net.IP, host string) (mainHost string) {
	v, ok := hp.table.Get(ip)
	switch h, _ := v.(*Hosts); {
	case !ok:
		// This is the first host for the ip.
		hp.table.Set(ip, &Hosts{Main: host})

		return host
	case h.Main == host:
		// This is a duplicate.  Go on.
	case h.Aliases == nil:
		// This is the first alias.
		h.Aliases = stringutil.NewSet(host)

		return h.Main
	case !h.Aliases.Has(host):
		// This is a new alias.
		h.Aliases.Add(host)

		return h.Main
	default:
		// This is a duplicate.  Go on.
	}

	return ""
}

// addPair puts the pair of ip and host to the rules builder if needed.  For
// each ip the first member of hosts will become the main one.
func (hp *hostsParser) addPairs(ip net.IP, hosts []string) {
	for _, host := range hosts {
		switch mainHost := hp.add(ip, host); mainHost {
		case "":
			// This host is a duplicate.
			continue
		case host:
			// This host is main.
			added, addedPtr := hp.writeMainRule(host, ip)
			hp.rules = append(hp.rules, ipRules{
				rule:    added,
				rulePtr: addedPtr,
				ip:      ip,
			})
		default:
			// This host is an alias.
			pair := fmt.Sprint(host, " ", mainHost)
			if hp.cnameSet.Has(pair) {
				continue
			}
			hp.writeAliasRule(host, mainHost)
			hp.cnameSet.Add(pair)
		}

		log.Debug("%s: added ip-host pair %q-%q", hostsContainerPref, ip, host)
	}
}

// writeAliasRule writes the CNAME rule for the alias-host pair into internal
// builders.
func (hp *hostsParser) writeAliasRule(alias, host string) {
	const (
		nl = "\n"
		sc = ";"

		rwSuccess = rules.MaskSeparator + "$dnsrewrite=NOERROR" + sc + "CNAME" + sc
		constLen  = len(rules.MaskPipe) + len(rwSuccess) + len(nl)
	)

	hp.rulesBuilder.Grow(constLen + len(host) + len(alias))
	stringutil.WriteToBuilder(hp.rulesBuilder, rules.MaskPipe, alias, rwSuccess, host, nl)
}

// writeMainRule writes the actual rule for the qtype and the PTR for the
// host-ip pair into internal builders.
func (hp *hostsParser) writeMainRule(host string, ip net.IP) (added, addedPtr string) {
	arpa, err := netutil.IPToReversedAddr(ip)
	if err != nil {
		return
	}

	const (
		nl = "\n"

		rwSuccess    = "^$dnsrewrite=NOERROR;"
		rwSuccessPTR = "^$dnsrewrite=NOERROR;PTR;"

		modLen    = len(rules.MaskPipe) + len(rwSuccess) + len(";")
		modLenPTR = len(rules.MaskPipe) + len(rwSuccessPTR)
	)

	var qtype string
	// The validation of the IP address has been performed earlier so it is
	// guaranteed to be either an IPv4 or an IPv6.
	if ip.To4() != nil {
		qtype = "A"
	} else {
		qtype = "AAAA"
	}

	ipStr := ip.String()
	fqdn := dns.Fqdn(host)

	ruleBuilder := &strings.Builder{}
	ruleBuilder.Grow(modLen + len(host) + len(qtype) + len(ipStr))
	stringutil.WriteToBuilder(
		ruleBuilder,
		rules.MaskPipe,
		host,
		rwSuccess,
		qtype,
		";",
		ipStr,
	)
	added = ruleBuilder.String()

	ruleBuilder.Reset()

	ruleBuilder.Grow(modLenPTR + len(arpa) + len(fqdn))
	stringutil.WriteToBuilder(ruleBuilder, rules.MaskPipe, arpa, rwSuccessPTR, fqdn)

	addedPtr = ruleBuilder.String()

	hp.rulesBuilder.Grow(len(added) + len(addedPtr) + 2*len(nl))
	stringutil.WriteToBuilder(hp.rulesBuilder, added, nl, addedPtr, nl)

	return added, addedPtr
}

// equalSet returns true if the internal hosts table just parsed equals target.
func (hp *hostsParser) equalSet(target *netutil.IPMap) (ok bool) {
	if target == nil {
		// hp.table shouldn't appear nil since it's initialized on each refresh.
		return target == hp.table
	}

	if hp.table.Len() != target.Len() {
		return false
	}

	hp.table.Range(func(ip net.IP, b interface{}) (cont bool) {
		// ok is set to true if the target doesn't contain ip or if the
		// appropriate hosts set isn't equal to the checked one, i.e. the main
		// hosts differ or the maps have at least one discrepancy.
		if a, hasIP := target.Get(ip); !hasIP {
			ok = true
		} else if hosts, aok := a.(*Hosts); aok {
			ok = !hosts.Equal(b.(*Hosts))
		}

		// Continue only if maps has no discrepancies.
		return !ok
	})

	// Return true if every value from the IP map has no discrepancies with the
	// appropriate one from the target.
	return !ok
}

// sendUpd tries to send the parsed data to the ch.
func (hp *hostsParser) sendUpd(ch chan *netutil.IPMap) {
	log.Debug("%s: sending upd", hostsContainerPref)

	upd := hp.table
	select {
	case ch <- upd:
		// Updates are delivered.  Go on.
	case <-ch:
		ch <- upd
		log.Debug("%s: replaced the last update", hostsContainerPref)
	case ch <- upd:
		// The previous update was just read and the next one pushed.  Go on.
	default:
		log.Error("%s: the updates channel is broken", hostsContainerPref)
	}
}

// newStrg creates a new rules storage from parsed data.
func (hp *hostsParser) newStrg(id int) (s *filterlist.RuleStorage, err error) {
	return filterlist.NewRuleStorage([]filterlist.RuleList{&filterlist.StringRuleList{
		ID:             id,
		RulesText:      hp.rulesBuilder.String(),
		IgnoreCosmetic: true,
	}})
}

// translations generates the map to translate $dnsrewrite rules to
// hosts-syntax ones.
func (hp *hostsParser) translations() (trans map[string]string) {
	l := len(hp.rules)
	if l == 0 {
		return nil
	}

	trans = make(map[string]string, l*2)
	for _, r := range hp.rules {
		v, ok := hp.table.Get(r.ip)
		if !ok {
			continue
		}

		var hosts *Hosts
		hosts, ok = v.(*Hosts)
		if !ok {
			continue
		}

		strs := append([]string{r.ip.String(), hosts.Main}, hosts.Aliases.Values()...)
		hostsLine := strings.Join(strs, " ")
		trans[r.rule], trans[r.rulePtr] = hostsLine, hostsLine
	}

	return trans
}

// refresh gets the data from specified files and propagates the updates if
// needed.
//
// TODO(e.burkov):  Accept a parameter to specify the files to refresh.
func (hc *HostsContainer) refresh() (err error) {
	log.Debug("%s: refreshing", hostsContainerPref)

	hp := hc.newHostsParser()
	if _, err = aghos.FileWalker(hp.parseFile).Walk(hc.fsys, hc.patterns...); err != nil {
		return fmt.Errorf("refreshing : %w", err)
	}

	if hp.equalSet(hc.last) {
		log.Debug("%s: no changes detected", hostsContainerPref)

		return nil
	}
	defer hp.sendUpd(hc.updates)

	hc.last = hp.table.ShallowClone()

	var rulesStrg *filterlist.RuleStorage
	if rulesStrg, err = hp.newStrg(hc.listID); err != nil {
		return fmt.Errorf("initializing rules storage: %w", err)
	}

	hc.resetEng(rulesStrg, hp.translations())

	return nil
}
