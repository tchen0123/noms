// @flow

// Copyright 2016 Attic Labs, Inc. All rights reserved.
// Licensed under the Apache License, version 2.0:
// http://www.apache.org/licenses/LICENSE-2.0

import argv from 'yargs';
import {
  Dataset,
  DatasetSpec,
} from '@attic/noms';

const args = argv
  .usage('Usage: $0 <dataset>')
  .command('dataset', 'dataset to read/write')
  .demand(1)
  .argv;

main().catch(ex => {
  console.error(ex.stack);
  process.exit(1);
});

async function main(): Promise<void> {
  const spec = DatasetSpec.parse(args._[0]);
  if (!spec) {
    process.stderr.write('invalid dataset spec');
    process.exit(1);
    return;
  }

  const ds = spec.dataset();
  await increment(ds);
}

async function increment(ds: Dataset): Promise<Dataset> {
  let lastVal = 0;

  const value = await ds.headValue();
  if (value !== null) {
    lastVal = Number(value);
  }

  const newVal = lastVal + 1;
  ds = await ds.commit(newVal);
  process.stdout.write(`${ newVal }\n`);
  return ds;
}
