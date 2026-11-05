// Writes goldens.json beside this file: for each pattern, whether
// Node's grep accepts it (new RegExp(p, 'i') and safe-regex) and which
// sample lines it matches. Run from the repository root, after `npm ci` in
// node/api:
//
//     node go/server/internal/jsregex/testdata/goldens.js

const fs = require('fs');
const path = require('path');

const api = path.resolve(__dirname, '../../../../../node/api');
const safeRegex = require(path.join(api, 'node_modules/safe-regex'));

const patterns = [
    // Everyday patterns
    'foo', 'foo|bar', '^## ', '\\bLLM-\\d+\\b', 'TODO.*fix', 'colou?r', '\\d{4}-\\d{2}-\\d{2}', '[A-Z][a-z]+',
    '\\s+$', '^\\s*$', '(?:auth|session)s?', 'a{2,}', 'a{2,3}', '\\.', '\\\\', '\\(x\\)', 'x\\*', '^$', '.',
    'hello world', 'HELLO', '\\w+@\\w+\\.com', '[^a-z]', '[\\w-]+', '[a\\-z]', '\\$\\d+', '^\\[\\d{2}:\\d{2} ',
    // Lookaround, groups, backreferences
    '(?<=v)\\d', '(?<!v)\\d', '(?=a)a', '(?!x)y', '(a)\\1', '(?<n>a)\\k<n>', '\\k<n>', '\\k', '(?<n>a)\\k',
    '\\1(a)', '\\8', '\\10', '(a)\\2', '\\0', '\\00', '\\012',
    // Escapes with Annex B meanings
    '\\cA', '\\ca', '\\c1', '\\c', '\\x41', '\\x4', '\\x', '\\u0041', '\\u12', '\\u', '\\p{L}', '\\P{L}',
    '\\A', '\\Z', '\\z', '\\G', '\\e', '\\a', '\\y', '\\-', '\\/', '\\ ', '\\B', '\\D', '\\W', '\\S', '\\f', '\\v',
    // Classes
    '[]', '[^]', '[]a]', '[\\b]', '[\\s]', '[\\S]', '[^\\s]', '[\\d\\s]', '[a-]', '[-a]', '[\\w-z]', '[z-a]',
    '[\\]]', '[[]', '[a-z&&[^b]]', '[.]', '[\\D]', '[^\\D]', '[a\\D]',
    // Quantifier oddities
    'a{', 'a{,3}', 'x{1', '{', '}', ']', 'a{3,1}', 'a**', '*a', '+', '?', 'a{2}?', 'a+?', 'a??', 'a*?',
    '(?:)', '()', '(', ')', '(?', '(?i)abc', '(?i:abc)', '(?#c)', '(?>a)', '(?<1a>x)', '(?<a>x)(?<a>y)',
    // Unsafe by safe-regex
    '(a+)+', '(a*)*', '(a|b)*c+', '(?:x+)+', '([a-z]+)*', '(a?){3}', 'a+b+c+d+e+f+g+h+i+j+k+l+m+n+o+p+q+r+s+t+u+v+w+x+y+z+',
    'a+b+c+d+e+f+g+h+i+j+k+l+m+n+o+p+q+r+s+t+u+v+w+x+y+', '(?=a+)+', '(?:a{2})+', '[a+]+', '\\d+\\s*\\w+',
    // Case and Unicode
    'école', 'ÉCOLE', 'straße', 'STRASSE', 'k', 'ǅ', '\\u212a', 'σ', 'Σ', 'İ', 'ı', 'ß', '[à-ÿ]', '\\u00e9',
    // Lines with \r and Unicode spaces
    'hello.$', 'hello$', '\\r', '\\s', 'a.b', '\\u2028', '[^]b',
    // Assertions, modifiers, named groups, legacy escapes, astral units
    '\\b+', '\\B*', '(?=a)+', '(?<=a)+', '\\k<z>(?<a>x)', '(?<a>x)|(?<a>y)', '(?<a>x)\\k<a>|(?<a>y)',
    '(?i-:a)', '(?-i:A)', '(?i:a)B', '(?m:^b)', '(?s:a.b)', '(a{,3})+', '(x{1)+', '\\c_', '[\\c_]', '[\\c1]',
    '[\\cA]', '\\08', '\\377', '\\400', '[\\1]', '[\\k]', '😀', '^.$', '^..$', '[😀]', '\\uD83D', 'x{2}{3}',
    '{1}', 'a|{2}', '(?:a|)+', '(a)?\\1b', '(?<n>a)(b)\\1', '(?<n>a)(b)\\2', '((a)|b)+\\2', '(?<a>x)(?<a>y)|z',
    '(?:(?<a>x)|(?<a>y))\\k<a>', '[\\d-z]', '[a-\\d]', '[%--]', '(?<$x>a)', '(?<_>a)', '(?<é>a)', 'a{99999999999}',
    'a{1,99999999999}', '(a)|\\1x', '\\B', 'x\\b', '^a|b$',
    // Capture reset per repetition, Canonicalize in backreferences, escaped
    // group names, empty removal list in a modifier group
    '^(a|(b))+\\2$', '^(?:(a)|b)+\\1$', '^(k)\\1$', '^(K)\\1$', '^(Σ)\\1$', '^(σ)\\1$', '^(s)\\1$',
    '(?<\\u0061>x)\\k<a>', '(?<\\u{61}b>x)\\k<ab>', '(?<a>x)\\k<\\u0061>', '(?<\\u0031>x)', '(?<\\uD835\\uDC9C>x)',
    '(?<\\u{110000}>x)', '(?<\\x61>x)', '(?im-:a)', '(?<=(a)\\1)b', '(?<=\\1(a))b', '(?<=a+)b', '(?<=(?=a)a)b',
    '((a)|b)+', '(a*)+?b', '(?:a|b)*?c', '(a)(?:\\1)*', 'x*?$', '(?:)*', '(a?)*b',
];

const lines = [
    'Hello World', 'hello\r', 'LLM-917 done', '  trailing  ', ' nbsp', 'ÉCOLE école', 'straße STRASSE',
    'Kelvin K k', 'a b', 'a\rb', 'tab\there', '', 'aaaa', 'x{', 'abc]def', '## Heading', '[10:00 work] hi',
    'colour color', '2026-11-05', 'TODO: fix it', 'auth sessions', 'vv1 x2', 'yx', 'aa', '$5 (x) x* \\ / -',
    'ǆ Ǆ ǅ', 'σ Σ ς', 'İ i ı I', 'mail@host.com', '\u0001A', 'z-a', 'end ', '\u000b\u000c',
    '😀', 'b', 'ab', 'AB', 'aB', 'xxxxxx', '%-', 'x1', 'bb', 'aab', 'xb', 'é1', 'A', 'a', 'x', 'é',
    'aba', 'kK', 'kk', 'ΣΣ', 'Σς', 'σς', 'sſ', 'xx', 'aab', 'aaab', 'bab', 'c', 'abac',
];

function grepOutcome(p) {
    // grepNotes' own checks, in its order.
    if (p.length > 200) return { error: 'length' };
    try {
        new RegExp(p, 'i');
    } catch (err) {
        return { error: 'invalid', message: err.message };
    }
    if (!safeRegex(p)) return { error: 'unsafe' };
    const re = new RegExp(p, 'i');
    return { matches: lines.map(l => re.test(l)) };
}

const out = {
    lines,
    patterns: patterns.map(p => ({ pattern: p, ...grepOutcome(p) })),
};
fs.writeFileSync(path.join(__dirname, 'goldens.json'), JSON.stringify(out, null, 2) + '\n');
console.log(out.patterns.filter(p => p.error).map(p => `${p.pattern} -> ${p.error}`).join('\n'));
