module github.com/raucheacho/agentkit/contrib/vectix

go 1.26.0

require (
	github.com/raucheacho/agentkit v0.0.0
	github.com/raucheacho/vectixdb/bindings/go v0.1.0
)

require (
	github.com/raucheacho/vectixdb/bindings/go/lib/darwin_amd64 v0.0.0 // indirect
	github.com/raucheacho/vectixdb/bindings/go/lib/darwin_arm64 v0.0.0 // indirect
	github.com/raucheacho/vectixdb/bindings/go/lib/linux_amd64 v0.0.0 // indirect
	github.com/raucheacho/vectixdb/bindings/go/lib/linux_arm64 v0.0.0 // indirect
)

// Développement : agentkit et vectixdb pris dans les dossiers voisins. Les
// bibliothèques statiques de vectixdb sont des modules à part, à remplacer
// aussi tant que ses versions publiées ne les embarquent pas.
replace (
	github.com/raucheacho/agentkit => ../..
	github.com/raucheacho/vectixdb/bindings/go => ../../../agentique/vectixdb/bindings/go
	github.com/raucheacho/vectixdb/bindings/go/lib/darwin_amd64 => ../../../agentique/vectixdb/bindings/go/lib/darwin_amd64
	github.com/raucheacho/vectixdb/bindings/go/lib/darwin_arm64 => ../../../agentique/vectixdb/bindings/go/lib/darwin_arm64
	github.com/raucheacho/vectixdb/bindings/go/lib/linux_amd64 => ../../../agentique/vectixdb/bindings/go/lib/linux_amd64
	github.com/raucheacho/vectixdb/bindings/go/lib/linux_arm64 => ../../../agentique/vectixdb/bindings/go/lib/linux_arm64
)
