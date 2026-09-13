// Native Firefox iOS 18.7 capture, September 5, 2026. Only keyboard and
// beforeinput/input events are replayed. keyText is the observed xterm key
// emission; dictation send attempts from the broken release are not an oracle.
export type NativeDictationEntry = Readonly<{
  t: number;
  kind: string;
  inputType?: string;
  data?: string | null;
  key?: string;
  keyText?: string;
  composing?: boolean;
  field: string;
  selection: readonly [number, number];
}>;

export const nativeDictationRestart: readonly NativeDictationEntry[] = [
  {
    "t": 131673,
    "kind": "beforeinput",
    "inputType": "insertText",
    "data": "Bes",
    "composing": false,
    "field": "​",
    "selection": [
      1,
      1
    ]
  },
  {
    "t": 132399,
    "kind": "beforeinput",
    "inputType": "insertText",
    "data": "test",
    "composing": false,
    "field": "",
    "selection": [
      0,
      0
    ]
  },
  {
    "t": 132405,
    "kind": "input",
    "inputType": "insertText",
    "data": "test",
    "composing": false,
    "field": "test",
    "selection": [
      4,
      4
    ]
  },
  {
    "t": 132412,
    "kind": "beforeinput",
    "inputType": "insertText",
    "data": " ",
    "composing": false,
    "field": "test",
    "selection": [
      4,
      4
    ]
  },
  {
    "t": 132412,
    "kind": "input",
    "inputType": "insertText",
    "data": " ",
    "composing": false,
    "field": "test ",
    "selection": [
      5,
      5
    ]
  },
  {
    "t": 132414,
    "kind": "beforeinput",
    "inputType": "insertText",
    "data": "1",
    "composing": false,
    "field": "test ",
    "selection": [
      5,
      5
    ]
  },
  {
    "t": 132414,
    "kind": "input",
    "inputType": "insertText",
    "data": "1",
    "composing": false,
    "field": "test 1",
    "selection": [
      6,
      6
    ]
  },
  {
    "t": 132414,
    "kind": "beforeinput",
    "inputType": "insertText",
    "data": "2",
    "composing": false,
    "field": "test 1",
    "selection": [
      6,
      6
    ]
  },
  {
    "t": 132414,
    "kind": "input",
    "inputType": "insertText",
    "data": "2",
    "composing": false,
    "field": "test 12",
    "selection": [
      7,
      7
    ]
  },
  {
    "t": 132414,
    "kind": "beforeinput",
    "inputType": "insertText",
    "data": "3",
    "composing": false,
    "field": "test 12",
    "selection": [
      7,
      7
    ]
  },
  {
    "t": 132414,
    "kind": "input",
    "inputType": "insertText",
    "data": "3",
    "composing": false,
    "field": "test 123",
    "selection": [
      8,
      8
    ]
  },
  {
    "t": 133418,
    "kind": "keydown",
    "key": " ",
    "composing": false,
    "field": "test 123",
    "selection": [
      8,
      8
    ],
    "keyText": " "
  },
  {
    "t": 133440,
    "kind": "beforeinput",
    "inputType": "insertText",
    "data": " ",
    "composing": false,
    "field": "test 123",
    "selection": [
      8,
      8
    ]
  },
  {
    "t": 133440,
    "kind": "input",
    "inputType": "insertText",
    "data": " ",
    "composing": false,
    "field": "test 123 ",
    "selection": [
      9,
      9
    ]
  },
  {
    "t": 136864,
    "kind": "beforeinput",
    "inputType": "insertText",
    "data": "b",
    "composing": false,
    "field": "test 123 ",
    "selection": [
      9,
      9
    ]
  },
  {
    "t": 136865,
    "kind": "input",
    "inputType": "insertText",
    "data": "b",
    "composing": false,
    "field": "test 123 b",
    "selection": [
      10,
      10
    ]
  },
  {
    "t": 136896,
    "kind": "beforeinput",
    "inputType": "insertText",
    "data": "bes",
    "composing": false,
    "field": "test 123 b",
    "selection": [
      9,
      10
    ]
  },
  {
    "t": 136897,
    "kind": "input",
    "inputType": "insertText",
    "data": "bes",
    "composing": false,
    "field": "test 123 bes",
    "selection": [
      12,
      12
    ]
  },
  {
    "t": 136931,
    "kind": "beforeinput",
    "inputType": "insertText",
    "data": "best",
    "composing": false,
    "field": "test 123 bes",
    "selection": [
      9,
      12
    ]
  },
  {
    "t": 136932,
    "kind": "input",
    "inputType": "insertText",
    "data": "best",
    "composing": false,
    "field": "test 123 best",
    "selection": [
      13,
      13
    ]
  },
  {
    "t": 137632,
    "kind": "beforeinput",
    "inputType": "insertText",
    "data": "best again",
    "composing": false,
    "field": "test 123 best",
    "selection": [
      9,
      13
    ]
  },
  {
    "t": 137635,
    "kind": "input",
    "inputType": "insertText",
    "data": "best again",
    "composing": false,
    "field": "test 123 best again",
    "selection": [
      19,
      19
    ]
  },
  {
    "t": 138297,
    "kind": "beforeinput",
    "inputType": "insertText",
    "data": "best again 12",
    "composing": false,
    "field": "test 123 best again",
    "selection": [
      9,
      19
    ]
  },
  {
    "t": 138298,
    "kind": "input",
    "inputType": "insertText",
    "data": "best again 12",
    "composing": false,
    "field": "test 123 best again 12",
    "selection": [
      22,
      22
    ]
  },
  {
    "t": 138597,
    "kind": "beforeinput",
    "inputType": "insertText",
    "data": "best again 123",
    "composing": false,
    "field": "test 123 best again 12",
    "selection": [
      9,
      22
    ]
  },
  {
    "t": 138597,
    "kind": "input",
    "inputType": "insertText",
    "data": "best again 123",
    "composing": false,
    "field": "test 123 best again 123",
    "selection": [
      23,
      23
    ]
  },
  {
    "t": 139146,
    "kind": "beforeinput",
    "inputType": "",
    "data": null,
    "composing": false,
    "field": "test 123 best again 123",
    "selection": [
      9,
      23
    ]
  },
  {
    "t": 139146,
    "kind": "input",
    "inputType": "",
    "data": null,
    "composing": false,
    "field": "test 123 best",
    "selection": [
      13,
      13
    ]
  },
  {
    "t": 139147,
    "kind": "beforeinput",
    "inputType": "insertText",
    "data": " ",
    "composing": false,
    "field": "test 123 best",
    "selection": [
      13,
      13
    ]
  },
  {
    "t": 139147,
    "kind": "input",
    "inputType": "insertText",
    "data": " ",
    "composing": false,
    "field": "test 123 best ",
    "selection": [
      14,
      14
    ]
  },
  {
    "t": 139147,
    "kind": "beforeinput",
    "inputType": "insertText",
    "data": "again",
    "composing": false,
    "field": "test 123 best ",
    "selection": [
      14,
      14
    ]
  },
  {
    "t": 139147,
    "kind": "input",
    "inputType": "insertText",
    "data": "again",
    "composing": false,
    "field": "test 123 best again",
    "selection": [
      19,
      19
    ]
  },
  {
    "t": 139147,
    "kind": "beforeinput",
    "inputType": "insertText",
    "data": " ",
    "composing": false,
    "field": "test 123 best again",
    "selection": [
      19,
      19
    ]
  },
  {
    "t": 139147,
    "kind": "input",
    "inputType": "insertText",
    "data": " ",
    "composing": false,
    "field": "test 123 best again ",
    "selection": [
      20,
      20
    ]
  },
  {
    "t": 139148,
    "kind": "beforeinput",
    "inputType": "insertText",
    "data": "1",
    "composing": false,
    "field": "test 123 best again ",
    "selection": [
      20,
      20
    ]
  },
  {
    "t": 139148,
    "kind": "input",
    "inputType": "insertText",
    "data": "1",
    "composing": false,
    "field": "test 123 best again 1",
    "selection": [
      21,
      21
    ]
  },
  {
    "t": 139148,
    "kind": "beforeinput",
    "inputType": "insertText",
    "data": "2",
    "composing": false,
    "field": "test 123 best again 1",
    "selection": [
      21,
      21
    ]
  },
  {
    "t": 139148,
    "kind": "input",
    "inputType": "insertText",
    "data": "2",
    "composing": false,
    "field": "test 123 best again 12",
    "selection": [
      22,
      22
    ]
  },
  {
    "t": 139148,
    "kind": "beforeinput",
    "inputType": "insertText",
    "data": "3",
    "composing": false,
    "field": "test 123 best again 12",
    "selection": [
      22,
      22
    ]
  },
  {
    "t": 139148,
    "kind": "input",
    "inputType": "insertText",
    "data": "3",
    "composing": false,
    "field": "test 123 best again 123",
    "selection": [
      23,
      23
    ]
  }
];

