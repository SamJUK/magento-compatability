/** @type {import('tailwindcss').Config} */
export default {
  content: ['./src/**/*.{astro,html,js,jsx,md,mdx,svelte,ts,tsx,vue}'],
  theme: {
    extend: {
      colors: {
        pass: {
          DEFAULT: '#10b981',
          light: '#d1fae5',
          dark: '#065f46',
        },
        fail: {
          DEFAULT: '#f43f5e',
          light: '#ffe4e6',
          dark: '#881337',
        },
        partial: {
          DEFAULT: '#f59e0b',
          light: '#fef3c7',
          dark: '#78350f',
        },
        unknown: {
          DEFAULT: '#71717a',
          light: '#f4f4f5',
          dark: '#27272a',
        },
      },
    },
  },
  plugins: [],
  darkMode: 'media',
};
