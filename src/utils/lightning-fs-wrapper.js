// Wrapper for @isomorphic-git/lightning-fs
import lightning_fsRaw from '../../node_modules/@isomorphic-git/lightning-fs/src/index.js';
const lightning_fs = lightning_fsRaw.default || lightning_fsRaw;
export default lightning_fs;
