package core

import (
	"database/sql"
	"errors"
	"fmt"
	"hash/fnv"
)

// Words for a session's default name (one word, e.g. otter, linus): short, lowercase a-z, some of
// them jokes, none a reserved address (isReserved). Changing the list changes only the names of
// new participants: a resumed session keeps the name stored in its row.
var (
	nameNouns = [...]string{
		"otter", "badger", "beaver", "bison", "camel", "cobra", "condor", "crane", "dingo", "dolphin",
		"donkey", "eagle", "egret", "falcon", "ferret", "finch", "fox", "gazelle", "gecko", "gerbil",
		"gibbon", "goose", "gopher", "heron", "hippo", "ibis", "iguana", "impala", "jackal", "jaguar",
		"koala", "lemur", "leopard", "lion", "llama", "lobster", "lynx", "magpie", "mallard", "manatee",
		"marmot", "marten", "meerkat", "mink", "mole", "moose", "narwhal", "newt", "ocelot", "okapi",
		"oriole", "osprey", "ostrich", "owl", "panda", "panther", "parrot", "pelican", "penguin",
		"pigeon", "plover", "puffin", "puma", "quail", "rabbit", "raven", "robin", "salmon", "seal",
		"shark", "sparrow", "squid", "stork", "swan", "tapir", "tiger", "toucan", "trout", "turtle",
		"walrus", "weasel", "whale", "wolf", "wombat", "wren", "yak", "zebra", "bee", "beetle",
		"cricket", "moth", "ant", "hare", "kiwi", "lark", "orca", "pony", "sloth", "snail", "tern",
		"toad", "vole", "acorn", "alder", "aspen", "birch", "cedar", "clover", "cypress", "daisy",
		"fern", "fig", "ginkgo", "hazel", "holly", "iris", "ivy", "juniper", "larch", "laurel", "lily",
		"lotus", "maple", "moss", "oak", "olive", "orchid", "palm", "pine", "poppy", "reed", "rose",
		"rowan", "sage", "spruce", "tulip", "willow", "yew", "bamboo", "basil", "cactus", "lichen",
		"lupin", "mango", "melon", "peach", "pear", "plum", "quince", "thyme", "bay", "beach", "brook",
		"canyon", "cape", "cliff", "coast", "cove", "creek", "delta", "dune", "fjord", "glade", "glen",
		"grove", "gulf", "harbor", "heath", "hill", "island", "lagoon", "lake", "marsh", "mesa", "moor",
		"oasis", "ocean", "pass", "peak", "plain", "pond", "prairie", "reef", "ridge", "river", "shore",
		"spring", "stream", "summit", "tundra", "valley", "vista", "aurora", "breeze", "cloud", "comet",
		"dawn", "dew", "dusk", "ember", "frost", "galaxy", "gale", "glow", "haze", "horizon", "meteor",
		"mist", "moon", "nebula", "nova", "orbit", "planet", "prism", "quasar", "rain", "rainbow",
		"shadow", "sky", "snow", "spark", "star", "storm", "sun", "thunder", "tide", "wave", "wind",
		"anchor", "anvil", "arrow", "atlas", "banner", "beacon", "bell", "bridge", "button", "candle",
		"canoe", "castle", "chalk", "compass", "copper", "crystal", "drum", "feather", "flag",
		"flute", "harp", "noodle", "pickle", "potato", "waffle", "taco", "nugget", "muffin", "burrito",
		"hamster", "goblin", "gremlin", "raccoon", "pudding", "toaster", "duck", "segfault", "hotfix",
		"kernel", "cache", "bug", "nit", "lgtm", "regex", "yolo", "nullptr", "elon", "dario", "tibo",
		"zuck", "sama", "satya", "sundar", "jensen", "bezos", "woz", "gates", "jobs", "linus", "lisa",
		"demis", "ilya", "andrej", "greg", "mira", "turing", "hopper", "lovelace", "knuth", "ritchie",
		"dijkstra",
	}
)

// wordStart is where ref's name starts in nameNouns: the same ref always gives the same word.
func wordStart(ref string) int {
	h := fnv.New64a()
	h.Write([]byte(ref))
	return int(h.Sum64() % uint64(len(nameNouns)))
}

// freeWord is the default name of a new session with ref in teamID ("" = a solo): the first free
// word (nameTaken) of nameNouns from wordStart(ref) on; when every word is taken, freeName's
// suffix on the start word.
func (t *txn) freeWord(teamID, ref string) (string, error) {
	start := wordStart(ref)
	for i := range len(nameNouns) {
		w := nameNouns[(start+i)%len(nameNouns)]
		taken, err := t.nameTaken(teamID, w, true)
		if err != nil || !taken {
			return w, err
		}
	}
	return t.freeName(teamID, nameNouns[start])
}

// formerName is the name the harness session ref had as a participant before, when it is to be a
// solo again (teamID ""): its newest person's participant's. That is a member of a team that
// closed (the session goes on as a solo in the same process). "" when there is
// none or the session joins a team.
func (t *txn) formerName(teamID, ref string) (string, error) {
	if teamID != "" {
		return "", nil
	}
	var name string
	err := t.QueryRowContext(t.ctx, `SELECT name FROM participants
		WHERE (harness_ref=? OR id IN (SELECT participant_id FROM participant_refs WHERE ref=?))
		AND person=1 ORDER BY created_at DESC, rowid DESC LIMIT 1`, ref, ref).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return name, internal(err)
}

// freeSoloName is name, or name-2, name-3, … while a live solo or an open team has it (the check
// freeWord makes: a send to the name must reach one).
func (t *txn) freeSoloName(name string) (string, error) {
	for i := 1; ; i++ {
		cand := name
		if i > 1 {
			cand = fmt.Sprintf("%s-%d", name, i)
		}
		taken, err := t.nameTaken("", cand, true)
		if err != nil || !taken {
			return cand, err
		}
	}
}
