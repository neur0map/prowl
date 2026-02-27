declare module 'kuzu-wasm' {
  interface KuzuModule {
    init(): Promise<void>;
    Database: typeof Database;
    Connection: typeof Connection;
    FS: VirtualFS;
  }

  interface VirtualFS {
    writeFile(path: string, data: string): Promise<void>;
    unlink(path: string): Promise<void>;
  }

  interface QueryResult {
    hasNext(): Promise<boolean>;
    getNext(): Promise<Record<string, unknown>>;
  }

  class Database {
    constructor(path: string);
    close(): Promise<void>;
  }

  class Connection {
    constructor(db: Database);
    query(cypher: string): Promise<QueryResult>;
    close(): Promise<void>;
  }

  function init(): Promise<void>;

  const kuzu: KuzuModule;
  export default kuzu;
  export { init, Database, Connection, type QueryResult, type VirtualFS };
}
