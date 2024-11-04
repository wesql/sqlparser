# SqlParser
Yet another SQL parser for MySQL.

`SqlParser` is a robust and efficient SQL parser tailored for MySQL syntax. 
It serves as a core component in various projects, enabling seamless SQL parsing and manipulation. 
Designed with performance and scalability in mind, `SqlParser` is suitable for both small-scale applications and large, complex systems.

# Release A New Version

To see all the versions: 
```bash
go list -m -versions github.com/wesql/sqlparser
```

to release a new version:
```bash
git tag ${version}
git push origin ${version}
```

## Usage

### WeScale Wasm Plugin

`SqlParser` is utilized by the [Wasm Plugin SDK](https://github.com/wesql/wescale-wasm-plugin-sdk) to enable dynamic SQL parsing within WebAssembly plugins. This integration allows developers to create custom plugins that can parse and manipulate SQL queries on the fly, enhancing the flexibility and functionality of your applications.

For detailed documentation on using `SqlParser` with Wasm plugins, refer to the [Wasm Plugin Documentation](https://wesql.io/docs/features/Wasm-Plugin).


# Acknowledgement
The backbone of this repo is extracted from [wesql/wescale](github.com/wesql/wescale) and [vitessio/vitess](https://github.com/vitessio/vitess).