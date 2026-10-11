// routeperf: list a tRPC router's procedures with their input JSON Schemas.
//   node extract.mjs <router module> <out.json> [exportName]
// Works with tRPC v10 and v11 (lazy routers too) and zod v3/v4; other
// validators are converted through Standard Schema's JSON Schema hook when
// they provide one.
import fs from 'node:fs';
import path from 'node:path';
import { createRequire } from 'node:module';
import { pathToFileURL } from 'node:url';

const [file, out, exportName] = process.argv.slice(2);

// Load .env files the way most dev servers do, without overriding the shell.
for (const f of ['.env.local', '.env.development.local', '.env.development', '.env']) {
  let text;
  try { text = fs.readFileSync(path.join(process.cwd(), f), 'utf8'); } catch { continue; }
  for (const line of text.split(/\r?\n/)) {
    const m = line.match(/^\s*(?:export\s+)?([A-Za-z_][A-Za-z0-9_]*)\s*=\s*(.*)\s*$/);
    if (!m || m[1] in process.env) continue;
    let v = m[2];
    if (/^(['"]).*\1$/.test(v)) v = v.slice(1, -1);
    else v = v.replace(/\s+#.*$/, '');
    process.env[m[1]] = v;
  }
}

const mod = await import(pathToFileURL(file).href);

const isRouter = (x) => !!(x && (typeof x === 'object' || typeof x === 'function') && x._def && (x._def.router || x._def.procedures));
function findRouter() {
  const cands = [];
  const seen = new Set();
  const visit = (name, v) => {
    if (!v || seen.has(v)) return;
    seen.add(v);
    if (isRouter(v)) cands.push([name, v]);
  };
  if (exportName) {
    const v = exportName === 'default' ? mod.default : mod[exportName];
    if (!isRouter(v)) throw new Error(`export "${exportName}" of ${file} is not a tRPC router`);
    return v;
  }
  visit('appRouter', mod.appRouter);
  for (const [k, v] of Object.entries(mod)) visit(k, v);
  if (mod.default && typeof mod.default === 'object') for (const [k, v] of Object.entries(mod.default)) visit(k, v);
  if (!cands.length) throw new Error(`${file} exports no tRPC router (export it, or name it with ${file}#exportName)`);
  // the app router is the one with the most procedures
  const count = (r) => Object.keys(r._def.procedures || {}).length + Object.keys(r._def.lazy || {}).length;
  cands.sort((a, b) => (b[0] === 'appRouter') - (a[0] === 'appRouter') || count(b[1]) - count(a[1]));
  return cands[0][1];
}

const router = findRouter();
const def = router._def;

// v11 lazy routers: load them all (loading adds their procedures)
for (let guard = 0; def.lazy && Object.keys(def.lazy).length && guard < 1000; guard++) {
  const key = Object.keys(def.lazy)[0];
  await def.lazy[key].load();
}

// zod v4's own converter, from the project's zod
let toJSONSchema;
try {
  const req = createRequire(path.join(process.cwd(), 'package.json'));
  const z = await import(pathToFileURL(req.resolve('zod')).href);
  toJSONSchema = z.toJSONSchema || z.z?.toJSONSchema;
} catch {}

function convert(schema) {
  if (!schema) return null;
  const std = schema['~standard'];
  if (std?.jsonSchema?.input) {
    try { return std.jsonSchema.input({ target: 'draft-2020-12' }); } catch {}
  }
  if (schema._zod && toJSONSchema) {
    try { return toJSONSchema(schema, { io: 'input', unrepresentable: 'any' }); } catch {}
  }
  if (schema._zod) return zod4(schema, 0);
  if (schema._def?.typeName) return zod3(schema, 0);
  return null;
}

// ---- zod v3 (_def.typeName)
function zod3(s, d) {
  if (!s || d > 12) return {};
  const def = s._def;
  const checks = def.checks || [];
  const ck = (kind) => checks.find((c) => c.kind === kind);
  switch (def.typeName) {
    case 'ZodString': {
      const o = { type: 'string' };
      for (const c of checks) {
        if (c.kind === 'min') o.minLength = c.value;
        else if (c.kind === 'max') o.maxLength = c.value;
        else if (c.kind === 'length') o.minLength = o.maxLength = c.value;
        else if (['email', 'uuid', 'url', 'datetime', 'date', 'time', 'cuid', 'cuid2', 'ulid', 'ip', 'emoji'].includes(c.kind))
          o.format = { datetime: 'date-time', url: 'uri' }[c.kind] || c.kind;
      }
      return o;
    }
    case 'ZodNumber': {
      const o = { type: ck('int') ? 'integer' : 'number' };
      for (const c of checks) {
        if (c.kind === 'min') o[c.inclusive ? 'minimum' : 'exclusiveMinimum'] = c.value;
        if (c.kind === 'max') o[c.inclusive ? 'maximum' : 'exclusiveMaximum'] = c.value;
      }
      return o;
    }
    case 'ZodBigInt': return { type: 'integer' };
    case 'ZodBoolean': return { type: 'boolean' };
    case 'ZodDate': return { type: 'string', format: 'date-time' };
    case 'ZodObject': {
      const shape = typeof def.shape === 'function' ? def.shape() : def.shape;
      const o = { type: 'object', properties: {}, required: [] };
      for (const [k, v] of Object.entries(shape || {})) {
        o.properties[k] = zod3(v, d + 1);
        if (!zod3Optional(v)) o.required.push(k);
      }
      return o;
    }
    case 'ZodArray': {
      const o = { type: 'array', items: zod3(def.type, d + 1) };
      if (def.minLength) o.minItems = def.minLength.value;
      if (def.maxLength) o.maxItems = def.maxLength.value;
      if (def.exactLength) o.minItems = o.maxItems = def.exactLength.value;
      return o;
    }
    case 'ZodSet': return { type: 'array', items: zod3(def.valueType, d + 1) };
    case 'ZodTuple': return { type: 'array', prefixItems: def.items.map((x) => zod3(x, d + 1)) };
    case 'ZodEnum': return { type: 'string', enum: def.values };
    case 'ZodNativeEnum': {
      const vals = Object.entries(def.values).filter(([k]) => isNaN(Number(k))).map(([, v]) => v);
      return { enum: vals };
    }
    case 'ZodLiteral': return { const: def.value };
    case 'ZodOptional': return zod3(def.innerType, d + 1);
    case 'ZodNullable': return { anyOf: [zod3(def.innerType, d + 1), { type: 'null' }] };
    case 'ZodDefault': return { ...zod3(def.innerType, d + 1), default: def.defaultValue() };
    case 'ZodCatch': case 'ZodReadonly': case 'ZodBranded':
      return zod3(def.innerType || def.type, d + 1);
    case 'ZodEffects': return zod3(def.schema, d + 1);
    case 'ZodPipeline': return zod3(def.in, d + 1);
    case 'ZodLazy': return zod3(def.getter(), d + 1);
    case 'ZodPromise': return zod3(def.type, d + 1);
    case 'ZodUnion': case 'ZodDiscriminatedUnion': {
      const opts = Array.isArray(def.options) ? def.options : [...def.options.values()];
      return { anyOf: opts.map((x) => zod3(x, d + 1)) };
    }
    case 'ZodIntersection': return { allOf: [zod3(def.left, d + 1), zod3(def.right, d + 1)] };
    case 'ZodRecord': case 'ZodMap': return { type: 'object' };
    case 'ZodNull': return { type: 'null' };
    default: return {};
  }
}
function zod3Optional(s) {
  const t = s?._def?.typeName;
  if (t === 'ZodOptional' || t === 'ZodDefault' || t === 'ZodUndefined' || t === 'ZodVoid') return true;
  if (t === 'ZodEffects') return zod3Optional(s._def.schema);
  if (t === 'ZodPipeline') return zod3Optional(s._def.in);
  if (t === 'ZodNullable' || t === 'ZodCatch' || t === 'ZodReadonly' || t === 'ZodBranded') return zod3Optional(s._def.innerType || s._def.type);
  return false;
}

// ---- zod v4 (_zod.def.type), when zod's own toJSONSchema isn't reachable
function zod4(s, d) {
  if (!s || !s._zod || d > 12) return {};
  const def = s._zod.def;
  const bag = s._zod.bag || {};
  switch (def.type) {
    case 'string': {
      const o = { type: 'string' };
      if (bag.format) o.format = { datetime: 'date-time', url: 'uri' }[bag.format] || bag.format;
      if (bag.minimum != null) o.minLength = bag.minimum;
      if (bag.maximum != null) o.maxLength = bag.maximum;
      return o;
    }
    case 'number': {
      const o = { type: bag.format && /int/.test(bag.format) ? 'integer' : 'number' };
      if (bag.minimum != null) o.minimum = bag.minimum;
      if (bag.exclusiveMinimum != null) o.exclusiveMinimum = bag.exclusiveMinimum;
      if (bag.maximum != null) o.maximum = bag.maximum;
      if (bag.exclusiveMaximum != null) o.exclusiveMaximum = bag.exclusiveMaximum;
      return o;
    }
    case 'int': return { type: 'integer' };
    case 'bigint': return { type: 'integer' };
    case 'boolean': return { type: 'boolean' };
    case 'date': return { type: 'string', format: 'date-time' };
    case 'object': {
      const o = { type: 'object', properties: {}, required: [] };
      for (const [k, v] of Object.entries(def.shape || {})) {
        o.properties[k] = zod4(v, d + 1);
        if (v._zod.optin !== 'optional') o.required.push(k);
      }
      return o;
    }
    case 'array': {
      const o = { type: 'array', items: zod4(def.element, d + 1) };
      if (bag.minimum != null) o.minItems = bag.minimum;
      if (bag.maximum != null) o.maxItems = bag.maximum;
      return o;
    }
    case 'tuple': return { type: 'array', prefixItems: def.items.map((x) => zod4(x, d + 1)) };
    case 'enum': return { enum: Object.values(def.entries) };
    case 'literal': return { enum: def.values };
    case 'optional': case 'nonoptional': case 'readonly': case 'catch': case 'prefault':
      return zod4(def.innerType, d + 1);
    case 'nullable': return { anyOf: [zod4(def.innerType, d + 1), { type: 'null' }] };
    case 'default': return { ...zod4(def.innerType, d + 1), default: typeof def.defaultValue === 'function' ? def.defaultValue() : def.defaultValue };
    case 'pipe': return zod4(def.in, d + 1);
    case 'lazy': return zod4(def.getter(), d + 1);
    case 'union': return { anyOf: def.options.map((x) => zod4(x, d + 1)) };
    case 'intersection': return { allOf: [zod4(def.left, d + 1), zod4(def.right, d + 1)] };
    case 'record': case 'map': return { type: 'object' };
    case 'null': return { type: 'null' };
    default: return {};
  }
}

function optionalRoot(schema) {
  if (!schema) return false;
  if (schema._zod) return schema._zod.optin === 'optional';
  if (schema._def?.typeName) return zod3Optional(schema);
  try { return schema['~standard']?.validate?.(undefined)?.issues === undefined; } catch { return false; }
}

// merge chained .input(a).input(b) object schemas
function merge(list) {
  if (list.length === 1) return list[0];
  if (list.some((s) => !s || s.type !== 'object')) return list.find(Boolean) || null;
  const o = { type: 'object', properties: {}, required: [] };
  for (const s of list) {
    Object.assign(o.properties, s.properties || {});
    o.required.push(...(s.required || []));
  }
  return o;
}

// superjson (and friends) serialize to {json, meta}; the default is identity
let transformer = '';
{
  const t = def._config?.transformer;
  const ser = t?.input?.serialize || t?.serialize;
  if (ser && !t._default) {
    try {
      const r = ser({ a: 1 });
      if (r && typeof r === 'object' && 'json' in r) transformer = 'superjson';
      else if (!(r && r.a === 1)) transformer = 'custom';
    } catch { transformer = 'custom'; }
  }
}

const procedures = [];
for (const [p, proc] of Object.entries(def.procedures || {})) {
  const pd = proc?._def;
  if (!pd) continue;
  const type = pd.type || (pd.query ? 'query' : pd.mutation ? 'mutation' : pd.subscription ? 'subscription' : 'unknown');
  const parsers = pd.inputs || [];
  const entry = { path: p, type };
  if (!parsers.length) entry.no_input = true;
  else {
    const conv = parsers.map(convert);
    if (conv.every(Boolean)) {
      const input = merge(conv);
      if (input && typeof input === 'object') {
        delete input.$schema;
        if (parsers.length === 1 && optionalRoot(parsers[0])) input['x-optional'] = true;
        entry.input = input;
      }
    }
  }
  if (pd.meta && typeof pd.meta === 'object') {
    try { entry.meta = JSON.parse(JSON.stringify(pd.meta)); } catch {}
  }
  procedures.push(entry);
}

const version = def.lazy !== undefined || Object.values(def.procedures || {}).some((p) => p?._def?.type) ? 11 : 10;
fs.writeFileSync(out, JSON.stringify({ trpc: { version, transformer, procedures } }, null, 2));
process.exit(0);
