package core

import "hash/fnv"

// Word lists for a session's default name (adjective-noun, e.g. amber-otter, grumpy-linus): short,
// lowercase a-z, 3-8 letters, some of them jokes. Changing a list changes only the names of
// new participants: a resumed session keeps the name stored in its row.
var (
	nameAdjectives = [...]string{
		"amber", "azure", "beige", "bronze", "cobalt", "coral", "cream", "crimson", "golden", "indigo",
		"ivory", "jade", "khaki", "lemon", "lilac", "lime", "maroon", "mauve", "mint", "navy", "ochre",
		"pearl", "ruby", "rust", "sandy", "scarlet", "silver", "slate", "teal", "topaz", "violet",
		"bright", "calm", "clear", "cool", "crisp", "dewy", "dusky", "early", "fair", "fresh", "frosty",
		"gentle", "glossy", "grassy", "hazy", "icy", "leafy", "light", "lucid", "mellow", "misty",
		"mossy", "muted", "pale", "quiet", "rainy", "rosy", "shady", "shiny", "silky", "smooth", "snowy",
		"soft", "sunny", "sunlit", "tidal", "vivid", "warm", "wavy", "windy", "woody", "airy", "balmy",
		"breezy", "cloudy", "foggy", "gusty", "humid", "lunar", "polar", "solar", "starry", "stormy",
		"summer", "autumn", "winter", "vernal", "wintry", "arctic", "alpine", "coastal", "desert",
		"forest", "meadow", "brave", "bold", "brisk", "busy", "candid", "clever", "cosy", "curious",
		"daring", "deft", "eager", "earnest", "easy", "fancy", "festive", "fleet", "fond", "frank",
		"frugal", "funny", "giddy", "glad", "grand", "handy", "happy", "hardy", "hearty", "helpful",
		"honest", "humble", "jaunty", "jolly", "jovial", "joyful", "keen", "kind", "lively", "lucky",
		"merry", "mighty", "modest", "neat", "nimble", "noble", "patient", "peppy", "perky", "plucky",
		"polite", "proud", "quick", "quirky", "ready", "robust", "rustic", "savvy", "serene", "sharp",
		"shy", "sincere", "snug", "spry", "stable", "steady", "sturdy", "swift", "tender", "thrifty",
		"tidy", "tough", "trusty", "upbeat", "valiant", "vital", "wise", "witty", "zany", "zesty",
		"agile", "ample", "big", "brief", "broad", "cheery", "civic", "compact", "cubic", "deep",
		"dense", "direct", "dual", "even", "exact", "extra", "faint", "famous", "firm", "flat", "fluent",
		"focal", "free", "full", "great", "hidden", "huge", "ideal", "inner", "jumbo", "just", "large",
		"lasting", "late", "latent", "level", "little", "local", "lofty", "long", "loyal", "main", "mid",
		"mobile", "modern", "narrow", "near", "new", "nice", "north", "novel", "odd", "open", "outer",
		"prime", "proper", "pure", "rapid", "rare", "real", "regal", "royal", "rural", "safe", "same",
		"scenic", "secret", "select", "short", "simple", "sleek", "slim", "slow", "small", "solid",
		"south", "spare", "sleepy", "grumpy", "sneaky", "cheeky", "wobbly", "fluffy", "sassy", "dizzy",
		"chunky", "soggy", "spicy", "bouncy", "cranky", "nerdy", "turbo", "goofy", "salty", "snacky",
		"feral", "smug", "chaotic", "based", "cursed", "spooky",
	}
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
		"canoe", "castle", "chalk", "compass", "copper", "crystal", "drum", "engine", "feather", "flag",
		"flute", "harp", "noodle", "pickle", "potato", "waffle", "taco", "nugget", "muffin", "burrito",
		"hamster", "goblin", "gremlin", "raccoon", "pudding", "toaster", "duck", "segfault", "hotfix",
		"kernel", "cache", "bug", "nit", "lgtm", "regex", "yolo", "nullptr", "elon", "dario", "tibo",
		"zuck", "sama", "satya", "sundar", "jensen", "bezos", "woz", "gates", "jobs", "linus", "lisa",
		"demis", "ilya", "andrej", "greg", "mira", "turing", "hopper", "lovelace", "knuth", "ritchie",
		"dijkstra",
	}
)

// wordName is the adjective-noun name for ref: the same ref always gives the same name, so a
// resumed session keeps it.
func wordName(ref string) string {
	h := fnv.New64a()
	h.Write([]byte(ref))
	x := h.Sum64()
	return nameAdjectives[x%uint64(len(nameAdjectives))] + "-" + nameNouns[(x/uint64(len(nameAdjectives)))%uint64(len(nameNouns))]
}
