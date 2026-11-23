module github.com/raucheacho/agentkit/contrib/vectix

go 1.26.0

require (
	github.com/raucheacho/agentkit v0.2.0
	github.com/raucheacho/vectixdb/bindings/go v0.1.0
)

require (
	github.com/raucheacho/vectixdb/bindings/go/lib/darwin_amd64 v0.1.0 // indirect
	github.com/raucheacho/vectixdb/bindings/go/lib/darwin_arm64 v0.1.0 // indirect
	github.com/raucheacho/vectixdb/bindings/go/lib/linux_amd64 v0.1.0 // indirect
	github.com/raucheacho/vectixdb/bindings/go/lib/linux_arm64 v0.1.0 // indirect
)

// Dans ce dépôt, contrib/vectix est construit avec l'agentkit voisin. Ce
// replace est ignoré par les projets qui dépendent de contrib/vectix : ils
// prennent la version requise ci-dessus.
replace github.com/raucheacho/agentkit => ../..
