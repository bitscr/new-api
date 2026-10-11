module.exports = {
  root: true,
  env: { browser: true, es2021: true, node: true },
  parserOptions: {
    ecmaVersion: 2020,
    sourceType: 'module',
    ecmaFeatures: { jsx: true },
  },
  plugins: ['header', 'react-hooks'],
  overrides: [
    {
      files: ['**/*.{js,jsx}'],
      rules: {
        'header/header': [
          2,
          'block',
          [
            '',
            'Copyright (C) 2026 bitscr',
            '',
            'This program is free software: you can redistribute it and/or modify',
            'it under the terms of the GNU Affero General Public License as',
            'published by the Free Software Foundation, either version 3 of the',
            'License, or (at your option) any later version.',
            '',
            'This program is distributed in the hope that it will be useful,',
            'but WITHOUT ANY WARRANTY; without even the implied warranty of',
            'MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the',
            'GNU Affero General Public License for more details.',
            '',
            'You should have received a copy of the GNU Affero General Public License',
            'along with this program. If not, see <https://www.gnu.org/licenses/>.',
            '',
            'For licensing inquiries, please open an issue at https://github.com/bitscr/new-api/issues',
            '',
          ],
        ],
        'no-multiple-empty-lines': ['error', { max: 1 }],
      },
    },
  ],
};
