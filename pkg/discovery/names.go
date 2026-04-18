package discovery

import (
	"fmt"
	"math/rand/v2"
)

var adjectives = []string{
	"Swift", "Cosmic", "Turbo", "Sneaky", "Velvet",
	"Golden", "Silent", "Fuzzy", "Neon", "Rusty",
	"Lunar", "Pixel", "Mystic", "Jolly", "Nimble",
	"Frosty", "Zesty", "Brave", "Quirky", "Witty",
	"Stormy", "Lucky", "Noble", "Mellow", "Fierce",
	"Gentle", "Rapid", "Calm", "Bold", "Vivid",
	"Cozy", "Dapper", "Epic", "Funky", "Groovy",
	"Happy", "Icy", "Jazzy", "Keen", "Lively",
	"Mighty", "Neat", "Plucky", "Royal", "Snappy",
	"Tiny", "Ultra", "Wacky", "Zippy", "Atomic",
	"Bouncy", "Crispy", "Dizzy", "Eager", "Fluffy",
	"Glossy", "Hasty", "Itchy", "Jumpy", "Kinky",
	"Lanky", "Moody", "Nutty", "Peppy", "Rocky",
	"Salty", "Tipsy", "Wonky", "Yappy", "Cloudy",
	"Dusty", "Fancy", "Giddy", "Hazy", "Inky",
}

var nouns = []string{
	"Penguin", "Waffle", "Raccoon", "Narwhal", "Panda",
	"Falcon", "Otter", "Badger", "Walrus", "Parrot",
	"Gecko", "Moose", "Ferret", "Toucan", "Bison",
	"Koala", "Sloth", "Raven", "Lemur", "Cobra",
	"Fox", "Wolf", "Bear", "Hawk", "Shark",
	"Tiger", "Eagle", "Crane", "Heron", "Lynx",
	"Puma", "Viper", "Squid", "Moth", "Wren",
	"Finch", "Stork", "Mole", "Newt", "Toad",
	"Quail", "Robin", "Snail", "Dingo", "Hyena",
	"Llama", "Camel", "Chimp", "Rogue", "Pirate",
	"Ninja", "Wizard", "Goblin", "Knight", "Viking",
	"Muffin", "Cookie", "Pickle", "Pretzel", "Donut",
	"Turnip", "Radish", "Noodle", "Dumpling", "Taco",
	"Rocket", "Comet", "Meteor", "Pulsar", "Photon",
	"Pebble", "Breeze", "Spark", "Ripple", "Ember",
}

func generateName() string {
	adj := adjectives[rand.IntN(len(adjectives))]
	noun := nouns[rand.IntN(len(nouns))]
	return fmt.Sprintf("%s %s", adj, noun)
}
