module github.com/t8y2/dbx/plugins/xxl-job/backend

go 1.22

require github.com/t8y2/dbx/plugins/sdk/go/dbx-plugin-sdk v0.0.0

// The DBX Go SDK is vendored into this repository so the plugin builds and
// packages standalone, without depending on a dbx monorepo checkout.
replace github.com/t8y2/dbx/plugins/sdk/go/dbx-plugin-sdk => ./dbx-plugin-sdk
