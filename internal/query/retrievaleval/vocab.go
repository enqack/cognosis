package retrievaleval

// Vocabulary for prose-like chunk text.
//
// The first corpus drew 8 tokens with replacement from a 12-token bag of
// synthetic strings ("lex007w03"). That was adequate for the vector leg, whose
// ground truth is geometric, and useless for the keyword leg:
//
//   - `websearch_to_tsquery` ANDs its terms, so a 5-term query needed all five
//     of ~6 distinct tokens present in a chunk. Measured result: the FTS leg
//     returned 0–2 candidates out of a requested 50, across every scope.
//   - Every chunk was the same length with term frequency ~1, so BM25's two
//     actual advantages over ts_rank_cd — saturating term frequency and
//     normalizing by document length — had nothing to act on. A comparison on
//     that corpus would have reported "no difference" for reasons that are an
//     artifact of the fixture, not a property of the rankers.
//   - Synthetic tokens also bypass the English stemmer and stopword list, so
//     the text config under test was barely exercised.
//
// Hence real words, disjoint per-cluster topics, and a shared background pool.

// topicWords holds one small vocabulary per cluster, kept disjoint so that
// cluster membership remains simultaneously the vector ground truth and a
// keyword ground truth — the property that lets the FTS leg be scored at all.
// Overlapping vocabularies would be more realistic and would destroy that,
// so the realism is spent on the background pool instead.
//
// Index is the cluster number; Build wraps with modulo when there are more
// clusters than topics.
var topicWords = [][]string{
	{"harbor", "anchor", "rigging", "mainsail", "keel", "starboard", "regatta", "mooring"},
	{"sourdough", "proofing", "kneading", "crumb", "levain", "scoring", "banneton", "hydration"},
	{"basalt", "sediment", "tectonic", "outcrop", "quartz", "erosion", "stratum", "moraine"},
	{"antibody", "pathogen", "vaccine", "immunity", "antigen", "lymphocyte", "titer", "serology"},
	{"aperture", "shutter", "bokeh", "exposure", "darkroom", "negative", "tripod", "focal"},
	{"fermentation", "hops", "malt", "wort", "yeast", "lager", "brewing", "carbonation"},
	{"glacier", "crevasse", "icefall", "seracs", "ablation", "firn", "meltwater", "terminus"},
	{"symphony", "cadence", "modulation", "counterpoint", "timbre", "crescendo", "fugue", "octave"},
	{"turbine", "rotor", "generator", "grid", "voltage", "transformer", "capacitor", "inverter"},
	{"orchard", "grafting", "pollination", "rootstock", "pruning", "cultivar", "blossom", "harvest"},
	{"kiln", "glaze", "porcelain", "throwing", "greenware", "slipware", "bisque", "stoneware"},
	{"nebula", "quasar", "redshift", "parallax", "corona", "pulsar", "ecliptic", "albedo"},
	{"loom", "warp", "weft", "selvage", "heddle", "shuttle", "twill", "brocade"},
	{"estuary", "tidal", "brackish", "mudflat", "salinity", "silt", "delta", "marsh"},
	{"cipher", "keypair", "entropy", "nonce", "handshake", "digest", "signature", "rotation"},
	{"vellum", "quire", "codex", "marginalia", "scriptorium", "colophon", "folio", "illumination"},
	{"lantern", "wick", "kerosene", "mantle", "flint", "tinder", "ember", "smolder"},
	{"tundra", "permafrost", "lichen", "caribou", "taiga", "boreal", "thaw", "peat"},
	{"scaffold", "girder", "cantilever", "truss", "buttress", "lintel", "keystone", "masonry"},
	{"pipette", "centrifuge", "reagent", "assay", "buffer", "titration", "eluent", "supernatant"},
	{"saffron", "cardamom", "turmeric", "fenugreek", "tamarind", "cumin", "paprika", "clove"},
	{"trawler", "gillnet", "bycatch", "spawning", "hatchery", "quota", "fishery", "longline"},
	{"aqueduct", "cistern", "culvert", "sluice", "weir", "reservoir", "conduit", "spillway"},
	{"embroidery", "chainstitch", "sampler", "hoop", "floss", "backing", "applique", "seam"},
	{"monsoon", "isobar", "cyclone", "humidity", "barometer", "squall", "downdraft", "gust"},
	{"foundry", "crucible", "ingot", "smelting", "alloy", "annealing", "quenching", "slag"},
	{"cartography", "isoline", "projection", "geodesy", "azimuth", "datum", "contour", "bearing"},
	{"apiary", "brood", "propolis", "swarm", "nectar", "forager", "comb", "royal"},
	{"typeface", "kerning", "ligature", "serif", "leading", "counter", "ascender", "hinting"},
	{"trellis", "compost", "mulch", "perennial", "seedling", "loam", "germination", "bolting"},
	{"sonar", "bathymetry", "trench", "hydrothermal", "abyssal", "submersible", "plume", "vent"},
	{"falconry", "jesses", "mews", "quarry", "hood", "stoop", "creance", "imping"},
	{"telegraph", "relay", "circuit", "keying", "repeater", "landline", "operator", "cable"},
	{"vineyard", "terroir", "tannin", "vintage", "pressing", "racking", "cellar", "varietal"},
	{"origami", "valley", "reverse", "squash", "crease", "petal", "sink", "tessellation"},
	{"caravan", "oasis", "dune", "saltpan", "wadi", "nomad", "escarpment", "mirage"},
	{"prosody", "meter", "caesura", "enjambment", "stanza", "trochee", "anapest", "refrain"},
	{"bindery", "signature", "endpaper", "headband", "spine", "casing", "sewing", "board"},
	{"radiator", "manifold", "camshaft", "throttle", "piston", "gasket", "flywheel", "tappet"},
	{"lighthouse", "fresnel", "beacon", "shoal", "reef", "buoy", "fog", "keeper"},
}

// backgroundWords are the connective tissue: words that appear across every
// cluster, at much higher frequency than any topic term. They are what give
// the corpus an IDF spread — without them every term is equally rare and
// nothing distinguishes one ranking function from another. Some are stopwords
// on purpose, so the text-search configuration is actually exercised rather
// than bypassed.
var backgroundWords = []string{
	"the", "and", "of", "to", "in", "that", "with", "for", "from", "over",
	"under", "after", "before", "between", "during", "against", "through",
	"work", "time", "place", "part", "case", "point", "state", "form", "line",
	"number", "group", "problem", "result", "change", "level", "order", "value",
	"process", "system", "method", "record", "surface", "measure", "pattern",
	"often", "usually", "rarely", "always", "never", "still", "already", "again",
	"small", "large", "early", "late", "long", "short", "high", "low", "open",
	"closed", "single", "common", "local", "general", "possible", "necessary",
	"begins", "ends", "holds", "carries", "leaves", "returns", "remains",
	"appears", "requires", "produces", "reduces", "follows", "depends",
}

// distinctiveWords are rare markers, one small set per chunk, disjoint from
// both topicWords and backgroundWords so a distinctive term can never leak in
// as ordinary prose. They exist to reproduce the real-vault failure this corpus
// otherwise cannot: terms that identify ONE section of ONE note.
//
// The head-drawn and tail-drawn query sets both fail to starve the keyword leg,
// and for a reason no draw strategy fixes. A chunk is 40-160 words at
// topicRate 0.30, so it draws 12-48 topic words from a 12-term cluster
// vocabulary — it contains nearly its entire vocabulary, and any conjunction
// over that vocabulary is satisfiable. Starvation needs terms that are rare
// *within a note*, not merely rare within a cluster.
//
// Real words, not synthetic tokens, for the reason recorded at the top of this
// file: synthetic strings bypass the English stemmer and stopword list and stop
// exercising the text config under test.
var distinctiveWords = []string{
	"alcove", "bramble", "cistern", "dovetail", "ember", "fathom", "gantry",
	"hearth", "inkwell", "jetty", "kiln", "lantern", "mantle", "nutmeg",
	"obelisk", "parapet", "quarry", "rampart", "sextant", "thicket", "urn",
	"vellum", "wharf", "yarrow", "zenith", "abacus", "bellows", "cobble",
	"dredge", "escarp", "fresco", "girder", "hoist", "ingot", "jamb",
	"kestrel", "lintel", "mortar", "nave", "oxbow", "plinth", "quill",
	"rafter", "spindle", "trestle", "underpass", "vestibule", "warren",
	"yoke", "zephyr", "arbor", "bastion", "culvert", "dowel", "eaves",
	"flume", "gable", "hutch", "islet", "joist", "kerb", "lattice",
	"millstone", "newel", "orchid", "pylon", "quoin", "reef", "sluice",
	"tannery", "usher", "vane", "weir", "yardarm", "aqueduct", "bulwark",
	"cornice", "dormer", "estuary", "ferrule", "grotto", "harrow", "inlet",
	"keystone", "louver", "mullion", "nook", "outcrop", "pediment", "quadrant",
	"ridgeline", "spandrel", "transom", "upland", "vault", "windlass",
	"anvil", "buttress", "cairn", "drawbridge", "escarpment", "furrow",
	"granary", "headland", "isthmus", "jetsam", "kelp", "lagoon", "marsh",
	"narrows", "oasis", "peninsula", "quagmire", "rivulet", "shoal",
	"tributary", "undertow", "vista", "watershed", "yonder", "zigzag",
}
